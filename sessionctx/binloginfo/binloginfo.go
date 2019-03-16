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

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/pd/client"
	"github.com/pingcap/tidb-tools/tidb-binlog/node"
	pumpcli "github.com/pingcap/tidb-tools/tidb-binlog/pump_client"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/timeutil"
	"github.com/pingcap/tipb/go-binlog"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

func init() {
	grpc.EnableTracing = false
}

// pumpsClient is the client to write binlog, it is opened on server start and never close,
// shared by all sessions.
var pumpsClient *pumpcli.PumpsClient
var pumpsClientLock sync.RWMutex

// BinlogInfo contains binlog data and binlog client.
type BinlogInfo struct {
	Data   *binlog.Binlog
	Client *pumpcli.PumpsClient
}

// GetPumpsClient get the pumps client instance.
func GetPumpsClient() *pumpcli.PumpsClient {
	pumpsClientLock.RLock()
	client := pumpsClient
	pumpsClientLock.RUnlock()
	return client
}

// GetPumpsClientByLogBin get the pumps client instance by log_bin variable.
func GetPumpsClientByLogBin() *pumpcli.PumpsClient {
	var binlogCli *pumpcli.PumpsClient
	tidbLogBin := variable.GetSysVar(variable.TiDBLogBin)
	logBin := variable.GetSysVar(variable.LogBin)
	if (logBin.Scope&variable.ScopeSession == 1 && logBin.Value == "1") ||
		(logBin.Value == "auto" && tidbLogBin.Scope&variable.ScopeSession == 1 && tidbLogBin.Value == "1") {
		atomic.StoreUint32(&variable.ProcessGeneralLog, 1)
		binlogCli = GetOrCreatePumpsClient()
	}
	return binlogCli
}

// GetOrCreatePumpsClient get or create the pumps client instance
func GetOrCreatePumpsClient() *pumpcli.PumpsClient {
	client := GetPumpsClient()
	if client == nil {
		var err error
		pumpsClientLock.RLock()
		client, err = CreatePumpsClient()
		pumpsClientLock.RUnlock()
		if err != nil {
			return nil
		}

		err = pumpcli.InitLogger(config.GetGlobalConfig().Log.ToLogConfig())
		if err != nil {
			return nil
		}
		SetPumpsClient(client)
	}
	return client
}

// CreatePumpsClient create pumps client
func CreatePumpsClient() (*pumpcli.PumpsClient, error) {
	var (
		err    error
		client *pumpcli.PumpsClient
	)
	securityOption := pd.SecurityOption{
		CAPath:   config.GetGlobalConfig().Security.ClusterSSLCA,
		CertPath: config.GetGlobalConfig().Security.ClusterSSLCert,
		KeyPath:  config.GetGlobalConfig().Security.ClusterSSLKey,
	}

	if len(config.GetGlobalConfig().Binlog.BinlogSocket) == 0 {
		client, err = pumpcli.NewPumpsClient(
			config.GetGlobalConfig().Path,
			timeutil.ParseDuration(config.GetGlobalConfig().Binlog.WriteTimeout),
			securityOption)
	} else {
		client, err = pumpcli.NewLocalPumpsClient(
			config.GetGlobalConfig().Path,
			config.GetGlobalConfig().Binlog.BinlogSocket,
			timeutil.ParseDuration(config.GetGlobalConfig().Binlog.WriteTimeout),
			securityOption)
	}
	return client, err
}

// SetPumpsClient sets the pumps client instance.
func SetPumpsClient(client *pumpcli.PumpsClient) {
	pumpsClientLock.Lock()
	pumpsClient = client
	pumpsClientLock.Unlock()
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

// ShouldEnableBinlog returns true if Binlog.AutoMode is false and Binlog.Enable is true, or Binlog.AutoMode is true and tidb_log_bin's value is "1"
func ShouldEnableBinlog() bool {
	if config.GetGlobalConfig().Binlog.AutoMode {
		return variable.SysVars[variable.TiDBLogBin].Value == "1"
	}

	return config.GetGlobalConfig().Binlog.Enable
}

// WriteBinlog writes a binlog to Pump.
func (info *BinlogInfo) WriteBinlog(clusterID uint64) error {
	skip := atomic.LoadUint32(&skipBinlog)
	if skip > 0 {
		metrics.CriticalErrorCounter.Add(1)
		return nil
	}

	if info.Client == nil {
		return errors.New("pumps client is nil")
	}

	// it will retry in PumpsClient if write binlog fail.
	err := info.Client.WriteBinlog(info.Data)
	if err != nil {
		log.Errorf("write binlog fail %v", errors.ErrorStack(err))
		if atomic.LoadUint32(&ignoreError) == 1 {
			log.Error("write binlog fail but error ignored")
			metrics.CriticalErrorCounter.Add(1)
			// If error happens once, we'll stop writing binlog.
			atomic.CompareAndSwapUint32(&skipBinlog, skip, skip+1)
			return nil
		}

		if strings.Contains(err.Error(), "received message larger than max") {
			// This kind of error is not critical, return directly.
			return errors.Errorf("binlog data is too large (%s)", err.Error())
		}

		return terror.ErrCritical.GenWithStackByArgs(err)
	}

	return nil
}

// SetDDLBinlog sets DDL binlog in the kv.Transaction.
func SetDDLBinlog(client *pumpcli.PumpsClient, txn kv.Transaction, jobID int64, ddlQuery string) {
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
		Client: client,
	}
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

// MockPumpsClient creates a PumpsClient, used for test.
func MockPumpsClient(client binlog.PumpClient) *pumpcli.PumpsClient {
	nodeID := "pump-1"
	pump := &pumpcli.PumpStatus{
		Status: node.Status{
			NodeID: nodeID,
			State:  node.Online,
		},
		Client: client,
	}

	pumpInfos := &pumpcli.PumpInfos{
		Pumps:            make(map[string]*pumpcli.PumpStatus),
		AvaliablePumps:   make(map[string]*pumpcli.PumpStatus),
		UnAvaliablePumps: make(map[string]*pumpcli.PumpStatus),
	}
	pumpInfos.Pumps[nodeID] = pump
	pumpInfos.AvaliablePumps[nodeID] = pump

	pCli := &pumpcli.PumpsClient{
		ClusterID:          1,
		Pumps:              pumpInfos,
		Selector:           pumpcli.NewSelector(pumpcli.Range),
		BinlogWriteTimeout: time.Second,
	}
	pCli.Selector.SetPumps([]*pumpcli.PumpStatus{pump})

	return pCli
}
