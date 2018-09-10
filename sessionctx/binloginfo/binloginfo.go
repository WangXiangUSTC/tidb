// Copyright 2016 PingCAP, Inc.
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

package binloginfo

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/terror"
	binlog "github.com/pingcap/tipb/go-binlog"
	log "github.com/sirupsen/logrus"
	//"golang.org/x/net/context"
	pClient "github.com/pingcap/tidb-tools/tidb-binlog/pump_client"
	"google.golang.org/grpc"
)

func init() {
	grpc.EnableTracing = false
}

var binlogWriteTimeout = 15 * time.Second

// pumpClient is the gRPC client to write binlog, it is opened on server start and never close,
// shared by all sessions.
//var pumpClient binlog.PumpClient
//var pumpClientLock sync.RWMutex
var pumpsClient *pClient.PumpsClient
var pumpsClientLock sync.RWMutex

// BinlogInfo contains binlog data and binlog client.
type BinlogInfo struct {
	Data *binlog.Binlog
	//Client binlog.PumpClient
	Client *pClient.PumpsClient
}

// GetPumpsClient gets the pump client instance.
func GetPumpsClient() *pClient.PumpsClient {
	pumpsClientLock.RLock()
	client := pumpsClient
	log.Infof("GetPumpsClient client is nil %v", client == nil)
	pumpsClientLock.RUnlock()
	if client == nil {
		return nil
	}
	return client
}

// SetPumpsClient sets the PumpsClient instance.
func SetPumpsClient(client *pClient.PumpsClient) {
	pumpsClientLock.Lock()
	log.Info("SetPumpsClient")
	pumpsClient = client
	pumpsClientLock.Unlock()
}

// SetGRPCTimeout sets grpc timeout for writing binlog.
func SetGRPCTimeout(timeout time.Duration) {
	if timeout < 300*time.Millisecond {
		log.Warnf("set binlog grpc timeout %s ignored, use default value %s", timeout, binlogWriteTimeout)
		return // Avoid invalid value
	}
	binlogWriteTimeout = timeout
}

// GetPrewriteValue gets binlog prewrite value in the context.
func GetPrewriteValue(ctx sessionctx.Context, createIfNotExists bool) *binlog.PrewriteValue {
	vars := ctx.GetSessionVars()
	v, ok := vars.TxnCtx.Binlog.(*binlog.PrewriteValue)
	if !ok && createIfNotExists {
		schemaVer := ctx.GetSessionVars().TxnCtx.SchemaVersion
		v = &binlog.PrewriteValue{SchemaVersion: schemaVer}
		vars.TxnCtx.Binlog = v
	}
	return v
}

var skipBinlog uint32
var ignoreError uint32

// DisableSkipBinlogFlag disable the skipBinlog flag.
func DisableSkipBinlogFlag() {
	atomic.StoreUint32(&skipBinlog, 0)
	log.Warn("[binloginfo] disable the skipBinlog flag")
}

// SetIgnoreError sets the ignoreError flag, this function called when TiDB start
// up and find config.Binlog.IgnoreError is true.
func SetIgnoreError(on bool) {
	if on {
		atomic.StoreUint32(&ignoreError, 1)
	} else {
		atomic.StoreUint32(&ignoreError, 0)
	}
}

// WriteBinlog writes a binlog to Pump.
func (info *BinlogInfo) WriteBinlog(clusterID uint64) error {
	skip := atomic.LoadUint32(&skipBinlog)
	if skip > 0 {
		metrics.CriticalErrorCounter.Add(1)
		return nil
	}

	if info.Client == nil {
		log.Error("pump client is nil")
		return errors.New("pump client is nil")
	}

	log.Debugf("begin write binlog, start ts: %d, type: %s", info.Data.StartTs, info.Data.Tp)
	err := info.Client.WriteBinlog(info.Data)
	log.Debugf("end write binlog, start ts: %d, type: %s", info.Data.StartTs, info.Data.Tp)
	if err != nil {
		log.Errorf("write binlog fail %v", errors.ErrorStack(err))
		if atomic.LoadUint32(&ignoreError) == 1 {
			log.Errorf("critical error, write binlog fail but error ignored: %s", errors.ErrorStack(err))
			metrics.CriticalErrorCounter.Add(1)
			// If error happens once, we'll stop writing binlog.
			atomic.CompareAndSwapUint32(&skipBinlog, skip, skip+1)
			return nil
		}

		return terror.ErrCritical.GenByArgs(err)
	}

	return nil
}

// SetDDLBinlog sets DDL binlog in the kv.Transaction.
func SetDDLBinlog(client interface{}, txn kv.Transaction, jobID int64, ddlQuery string) {
	log.Infof("SetDDLBinlog client is nil %v", client)
	if client == nil {
		return
	}
	ddlQuery = addSpecialComment(ddlQuery)
	info := &BinlogInfo{
		Data: &binlog.Binlog{
			Tp:       binlog.BinlogType_Prewrite,
			DdlJobId: jobID,
			DdlQuery: []byte(ddlQuery),
		},
		Client: client.(*pClient.PumpsClient),
	}
	log.Info("txn.SetOption(kv.BinlogInfo, info)")
	txn.SetOption(kv.BinlogInfo, info)
}

const specialPrefix = `/*!90000 `

func addSpecialComment(ddlQuery string) string {
	if strings.Contains(ddlQuery, specialPrefix) {
		return ddlQuery
	}
	upperQuery := strings.ToUpper(ddlQuery)
	reg, err := regexp.Compile(`SHARD_ROW_ID_BITS\s*=\s*\d+`)
	terror.Log(err)
	loc := reg.FindStringIndex(upperQuery)
	if len(loc) < 2 {
		return ddlQuery
	}
	return ddlQuery[:loc[0]] + specialPrefix + ddlQuery[loc[0]:loc[1]] + ` */` + ddlQuery[loc[1]:]
}
