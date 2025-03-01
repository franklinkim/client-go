// Copyright 2021 TiKV Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

// NOTE: The code in this file is based on code from the
// TiDB project, licensed under the Apache License v 2.0
//
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/prewrite.go
//

// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tikv

import (
	"encoding/hex"
	"math"
	"sync/atomic"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/client-go/v2/config"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/internal/client"
	"github.com/tikv/client-go/v2/internal/locate"
	"github.com/tikv/client-go/v2/internal/logutil"
	"github.com/tikv/client-go/v2/internal/retry"
	"github.com/tikv/client-go/v2/metrics"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/util"
	"go.uber.org/zap"
)

type actionPrewrite struct{ retry bool }

var _ twoPhaseCommitAction = actionPrewrite{}

func (actionPrewrite) String() string {
	return "prewrite"
}

func (actionPrewrite) tiKVTxnRegionsNumHistogram() prometheus.Observer {
	return metrics.TxnRegionsNumHistogramPrewrite
}

func (c *twoPhaseCommitter) buildPrewriteRequest(batch batchMutations, txnSize uint64) *tikvrpc.Request {
	m := batch.mutations
	mutations := make([]*kvrpcpb.Mutation, m.Len())
	isPessimisticLock := make([]bool, m.Len())
	for i := 0; i < m.Len(); i++ {
		mutations[i] = &kvrpcpb.Mutation{
			Op:    m.GetOp(i),
			Key:   m.GetKey(i),
			Value: m.GetValue(i),
		}
		isPessimisticLock[i] = m.IsPessimisticLock(i)
	}
	c.mu.Lock()
	minCommitTS := c.minCommitTS
	c.mu.Unlock()
	if c.forUpdateTS > 0 && c.forUpdateTS >= minCommitTS {
		minCommitTS = c.forUpdateTS + 1
	} else if c.startTS >= minCommitTS {
		minCommitTS = c.startTS + 1
	}

	if val, err := util.EvalFailpoint("mockZeroCommitTS"); err == nil {
		// Should be val.(uint64) but failpoint doesn't support that.
		if tmp, ok := val.(int); ok && uint64(tmp) == c.startTS {
			minCommitTS = 0
		}
	}

	ttl := c.lockTTL

	if c.sessionID > 0 {
		if _, err := util.EvalFailpoint("twoPCShortLockTTL"); err == nil {
			ttl = 1
			keys := make([]string, 0, len(mutations))
			for _, m := range mutations {
				keys = append(keys, hex.EncodeToString(m.Key))
			}
			logutil.BgLogger().Info("[failpoint] injected lock ttl = 1 on prewrite",
				zap.Uint64("txnStartTS", c.startTS), zap.Strings("keys", keys))
		}
	}

	req := &kvrpcpb.PrewriteRequest{
		Mutations:         mutations,
		PrimaryLock:       c.primary(),
		StartVersion:      c.startTS,
		LockTtl:           ttl,
		IsPessimisticLock: isPessimisticLock,
		ForUpdateTs:       c.forUpdateTS,
		TxnSize:           txnSize,
		MinCommitTs:       minCommitTS,
		MaxCommitTs:       c.maxCommitTS,
	}

	if _, err := util.EvalFailpoint("invalidMaxCommitTS"); err == nil {
		if req.MaxCommitTs > 0 {
			req.MaxCommitTs = minCommitTS - 1
		}
	}

	if c.isAsyncCommit() {
		if batch.isPrimary {
			req.Secondaries = c.asyncSecondaries()
		}
		req.UseAsyncCommit = true
	}

	if c.isOnePC() {
		req.TryOnePc = true
	}

	return tikvrpc.NewRequest(tikvrpc.CmdPrewrite, req, kvrpcpb.Context{Priority: c.priority, SyncLog: c.syncLog, ResourceGroupTag: c.resourceGroupTag})
}

func (action actionPrewrite) handleSingleBatch(c *twoPhaseCommitter, bo *Backoffer, batch batchMutations) (err error) {
	// WARNING: This function only tries to send a single request to a single region, so it don't
	// need to unset the `useOnePC` flag when it fails. A special case is that when TiKV returns
	// regionErr, it's uncertain if the request will be splitted into multiple and sent to multiple
	// regions. It invokes `prewriteMutations` recursively here, and the number of batches will be
	// checked there.

	if c.sessionID > 0 {
		if batch.isPrimary {
			if _, err := util.EvalFailpoint("prewritePrimaryFail"); err == nil {
				// Delay to avoid cancelling other normally ongoing prewrite requests.
				time.Sleep(time.Millisecond * 50)
				logutil.Logger(bo.GetCtx()).Info("[failpoint] injected error on prewriting primary batch",
					zap.Uint64("txnStartTS", c.startTS))
				return errors.New("injected error on prewriting primary batch")
			}
			util.EvalFailpoint("prewritePrimary") // for other failures like sleep or pause
		} else {
			if _, err := util.EvalFailpoint("prewriteSecondaryFail"); err == nil {
				// Delay to avoid cancelling other normally ongoing prewrite requests.
				time.Sleep(time.Millisecond * 50)
				logutil.Logger(bo.GetCtx()).Info("[failpoint] injected error on prewriting secondary batch",
					zap.Uint64("txnStartTS", c.startTS))
				return errors.New("injected error on prewriting secondary batch")
			}
			util.EvalFailpoint("prewriteSecondary") // for other failures like sleep or pause
		}
	}

	txnSize := uint64(c.regionTxnSize[batch.region.GetID()])
	// When we retry because of a region miss, we don't know the transaction size. We set the transaction size here
	// to MaxUint64 to avoid unexpected "resolve lock lite".
	if action.retry {
		txnSize = math.MaxUint64
	}

	tBegin := time.Now()
	attempts := 0

	req := c.buildPrewriteRequest(batch, txnSize)
	sender := NewRegionRequestSender(c.store.regionCache, c.store.GetTiKVClient())
	defer func() {
		if err != nil {
			// If we fail to receive response for async commit prewrite, it will be undetermined whether this
			// transaction has been successfully committed.
			// If prewrite has been cancelled, all ongoing prewrite RPCs will become errors, we needn't set undetermined
			// errors.
			if (c.isAsyncCommit() || c.isOnePC()) && sender.GetRPCError() != nil && atomic.LoadUint32(&c.prewriteCancelled) == 0 {
				c.setUndeterminedErr(errors.Trace(sender.GetRPCError()))
			}
		}
	}()
	for {
		attempts++
		if time.Since(tBegin) > slowRequestThreshold {
			logutil.BgLogger().Warn("slow prewrite request", zap.Uint64("startTS", c.startTS), zap.Stringer("region", &batch.region), zap.Int("attempts", attempts))
			tBegin = time.Now()
		}

		resp, err := sender.SendReq(bo, req, batch.region, client.ReadTimeoutShort)
		// Unexpected error occurs, return it
		if err != nil {
			return errors.Trace(err)
		}

		regionErr, err := resp.GetRegionError()
		if err != nil {
			return errors.Trace(err)
		}
		if regionErr != nil {
			// For other region error and the fake region error, backoff because
			// there's something wrong.
			// For the real EpochNotMatch error, don't backoff.
			if regionErr.GetEpochNotMatch() == nil || locate.IsFakeRegionError(regionErr) {
				err = bo.Backoff(retry.BoRegionMiss, errors.New(regionErr.String()))
				if err != nil {
					return errors.Trace(err)
				}
			}
			same, err := batch.relocate(bo, c.store.regionCache)
			if err != nil {
				return errors.Trace(err)
			}
			if same {
				continue
			}
			err = c.doActionOnMutations(bo, actionPrewrite{true}, batch.mutations)
			return errors.Trace(err)
		}

		if resp.Resp == nil {
			return errors.Trace(tikverr.ErrBodyMissing)
		}
		prewriteResp := resp.Resp.(*kvrpcpb.PrewriteResponse)
		keyErrs := prewriteResp.GetErrors()
		if len(keyErrs) == 0 {
			// Clear the RPC Error since the request is evaluated successfully.
			sender.SetRPCError(nil)

			if batch.isPrimary {
				// After writing the primary key, if the size of the transaction is larger than 32M,
				// start the ttlManager. The ttlManager will be closed in tikvTxn.Commit().
				// In this case 1PC is not expected to be used, but still check it for safety.
				if int64(c.txnSize) > config.GetGlobalConfig().TiKVClient.TTLRefreshedTxnSize &&
					prewriteResp.OnePcCommitTs == 0 {
					c.run(c, nil)
				}
			}

			if c.isOnePC() {
				if prewriteResp.OnePcCommitTs == 0 {
					if prewriteResp.MinCommitTs != 0 {
						return errors.Trace(errors.New("MinCommitTs must be 0 when 1pc falls back to 2pc"))
					}
					logutil.Logger(bo.GetCtx()).Warn("1pc failed and fallbacks to normal commit procedure",
						zap.Uint64("startTS", c.startTS))
					metrics.OnePCTxnCounterFallback.Inc()
					c.setOnePC(false)
					c.setAsyncCommit(false)
				} else {
					// For 1PC, there's no racing to access to access `onePCCommmitTS` so it's safe
					// not to lock the mutex.
					if c.onePCCommitTS != 0 {
						logutil.Logger(bo.GetCtx()).Fatal("one pc happened multiple times",
							zap.Uint64("startTS", c.startTS))
					}
					c.onePCCommitTS = prewriteResp.OnePcCommitTs
				}
				return nil
			} else if prewriteResp.OnePcCommitTs != 0 {
				logutil.Logger(bo.GetCtx()).Fatal("tikv committed a non-1pc transaction with 1pc protocol",
					zap.Uint64("startTS", c.startTS))
			}
			if c.isAsyncCommit() {
				// 0 if the min_commit_ts is not ready or any other reason that async
				// commit cannot proceed. The client can then fallback to normal way to
				// continue committing the transaction if prewrite are all finished.
				if prewriteResp.MinCommitTs == 0 {
					if c.testingKnobs.noFallBack {
						return nil
					}
					logutil.Logger(bo.GetCtx()).Warn("async commit cannot proceed since the returned minCommitTS is zero, "+
						"fallback to normal path", zap.Uint64("startTS", c.startTS))
					c.setAsyncCommit(false)
				} else {
					c.mu.Lock()
					if prewriteResp.MinCommitTs > c.minCommitTS {
						c.minCommitTS = prewriteResp.MinCommitTs
					}
					c.mu.Unlock()
				}
			}
			return nil
		}
		var locks []*Lock
		for _, keyErr := range keyErrs {
			// Check already exists error
			if alreadyExist := keyErr.GetAlreadyExist(); alreadyExist != nil {
				e := &tikverr.ErrKeyExist{AlreadyExist: alreadyExist}
				return c.extractKeyExistsErr(e)
			}

			// Extract lock from key error
			lock, err1 := extractLockFromKeyErr(keyErr)
			if err1 != nil {
				return errors.Trace(err1)
			}
			logutil.BgLogger().Info("prewrite encounters lock",
				zap.Uint64("session", c.sessionID),
				zap.Stringer("lock", lock))
			locks = append(locks, lock)
		}
		start := time.Now()
		msBeforeExpired, err := c.store.lockResolver.resolveLocksForWrite(bo, c.startTS, locks)
		if err != nil {
			return errors.Trace(err)
		}
		atomic.AddInt64(&c.getDetail().ResolveLockTime, int64(time.Since(start)))
		if msBeforeExpired > 0 {
			err = bo.BackoffWithCfgAndMaxSleep(retry.BoTxnLock, int(msBeforeExpired), errors.Errorf("2PC prewrite lockedKeys: %d", len(locks)))
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
}

func (c *twoPhaseCommitter) prewriteMutations(bo *Backoffer, mutations CommitterMutations) error {
	if span := opentracing.SpanFromContext(bo.GetCtx()); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("twoPhaseCommitter.prewriteMutations", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		bo.SetCtx(opentracing.ContextWithSpan(bo.GetCtx(), span1))
	}

	// `doActionOnMutations` will unset `useOnePC` if the mutations is splitted into multiple batches.
	return c.doActionOnMutations(bo, actionPrewrite{}, mutations)
}
