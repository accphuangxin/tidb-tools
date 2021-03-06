// Copyright 2018 PingCAP, Inc.
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

package diff

import (
	"container/heap"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/tidb-tools/pkg/dbutil"
	"github.com/pingcap/tidb-tools/pkg/utils"
	"go.uber.org/zap"
)

// TableInstance record a table instance
type TableInstance struct {
	Conn       *sql.DB `json:"-"`
	Schema     string  `json:"schema"`
	Table      string  `json:"table"`
	InstanceID string  `json:"instance-id"`
	info       *model.TableInfo
}

// TableDiff saves config for diff table
type TableDiff struct {
	// source tables
	SourceTables []*TableInstance `json:"source-tables"`
	// target table
	TargetTable *TableInstance `json:"target-table"`

	// columns be ignored
	IgnoreColumns []string `json:"-"`

	// columns be removed
	RemoveColumns []string `json:"-"`

	// field should be the primary key, unique key or field with index
	Fields string `json:"fields"`

	// select range, for example: "age > 10 AND age < 20"
	Range string `json:"range"`

	// for example, the whole data is [1...100]
	// we can split these data to [1...10], [11...20], ..., [91...100]
	// the [1...10] is a chunk, and it's chunk size is 10
	// size of the split chunk
	ChunkSize int `json:"chunk-size"`

	// sampling check percent, for example 10 means only check 10% data
	Sample int `json:"sample"`

	// how many goroutines are created to check data
	CheckThreadCount int `json:"-"`

	// set true if target-db and source-db all support tidb implicit column "_tidb_rowid"
	UseRowID bool `json:"use-rowid"`

	// set false if want to comapre the data directly
	UseChecksum bool `json:"-"`

	// set true if just want compare data by checksum, will skip select data when checksum is not equal
	OnlyUseChecksum bool `json:"-"`

	// collation config in mysql/tidb, should corresponding to charset.
	Collation string `json:"collation"`

	// ignore check table's struct
	IgnoreStructCheck bool `json:"-"`

	// ignore check table's data
	IgnoreDataCheck bool `json:"-"`

	// set true will continue check from the latest checkpoint
	UseCheckpoint bool `json:"use-checkpoint"`

	// get tidb statistics information from which table instance. if is nil, will split chunk by random.
	TiDBStatsSource *TableInstance `json:"tidb-stats-source"`

	sqlCh chan string

	wg sync.WaitGroup

	configHash string
}

func (t *TableDiff) setConfigHash() error {
	jsonBytes, err := json.Marshal(t)
	if err != nil {
		return errors.Trace(err)
	}

	t.configHash = fmt.Sprintf("%x", md5.Sum(jsonBytes))
	log.Debug("sync-diff-inspector config", zap.ByteString("config", jsonBytes), zap.String("hash", t.configHash))

	return nil
}

// Equal tests whether two database have same data and schema.
func (t *TableDiff) Equal(ctx context.Context, writeFixSQL func(string) error) (bool, bool, error) {
	t.adjustConfig()

	t.sqlCh = make(chan string)

	stopWriteSqlsCh := t.WriteSqls(ctx, writeFixSQL)
	stopUpdateSummaryCh := t.UpdateSummaryInfo(ctx)

	err := t.getTableInfo(ctx)
	if err != nil {
		return false, false, errors.Trace(err)
	}

	structEqual := true
	dataEqual := true

	if !t.IgnoreStructCheck {
		structEqual, err = t.CheckTableStruct(ctx)
		if err != nil {
			return false, false, errors.Trace(err)
		}
	}

	if !t.IgnoreDataCheck {
		dataEqual, err = t.CheckTableData(ctx)
		if err != nil {
			return false, false, errors.Trace(err)
		}
	}

	stopWriteSqlsCh <- true
	stopUpdateSummaryCh <- true

	t.wg.Wait()
	return structEqual, dataEqual, nil
}

// CheckTableStruct checks table's struct
func (t *TableDiff) CheckTableStruct(ctx context.Context) (bool, error) {
	for _, sourceTable := range t.SourceTables {
		eq := dbutil.EqualTableInfo(sourceTable.info, t.TargetTable.info)
		if !eq {
			return false, nil
		}
	}

	return true, nil
}

func (t *TableDiff) adjustConfig() {
	if t.ChunkSize <= 0 {
		t.ChunkSize = 100
	}

	if len(t.Range) == 0 {
		t.Range = "TRUE"
	}
	if t.Sample <= 0 {
		t.Sample = 100
	}

	if t.CheckThreadCount <= 0 {
		t.CheckThreadCount = 4
	}
}

func (t *TableDiff) getTableInfo(ctx context.Context) error {
	tableInfo, err := dbutil.GetTableInfoWithRowID(ctx, t.TargetTable.Conn, t.TargetTable.Schema, t.TargetTable.Table, t.UseRowID)
	if err != nil {
		return errors.Trace(err)
	}
	t.TargetTable.info = removeColumns(tableInfo, t.RemoveColumns)

	for _, sourceTable := range t.SourceTables {
		tableInfo, err := dbutil.GetTableInfoWithRowID(ctx, sourceTable.Conn, sourceTable.Schema, sourceTable.Table, t.UseRowID)
		if err != nil {
			return errors.Trace(err)
		}
		sourceTable.info = removeColumns(tableInfo, t.RemoveColumns)
	}

	return nil
}

// CheckTableData checks table's data
func (t *TableDiff) CheckTableData(ctx context.Context) (equal bool, err error) {
	table := t.TargetTable

	useTiDB := false
	if t.TiDBStatsSource != nil {
		table = t.TiDBStatsSource
		useTiDB = true
	}

	fromCheckpoint := true
	chunks, err := t.LoadCheckpoint(ctx)
	if err != nil {
		return false, errors.Trace(err)
	}

	if len(chunks) == 0 {
		log.Debug("don't have checkpoint info or config changed")

		fromCheckpoint = false
		chunks, err = SplitChunks(ctx, table, t.Fields, t.Range, t.ChunkSize, t.Collation, useTiDB)
	}

	if len(chunks) == 0 {
		log.Warn("get 0 chunks, table is not checked", zap.String("table", dbutil.TableName(t.TargetTable.Schema, t.TargetTable.Table)))
		return true, nil
	}

	checkResultCh := make(chan bool, t.CheckThreadCount)
	defer close(checkResultCh)

	checkWorkerCh := make([]chan *ChunkRange, 0, t.CheckThreadCount)
	for i := 0; i < t.CheckThreadCount; i++ {
		checkWorkerCh = append(checkWorkerCh, make(chan *ChunkRange, 10))
		go t.checkChunksDataEqual(ctx, t.Sample < 100 && !fromCheckpoint, checkWorkerCh[i], checkResultCh)
	}

	go func() {
		defer func() {
			for _, ch := range checkWorkerCh {
				close(ch)
			}
		}()

		for _, chunk := range chunks {
			select {
			case checkWorkerCh[chunk.ID%t.CheckThreadCount] <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	checkedNum := 0
	equal = true

CheckResult:
	for {
		select {
		case eq := <-checkResultCh:
			checkedNum++
			if !eq {
				equal = false
			}
			if len(chunks) == checkedNum {
				break CheckResult
			}
		case <-ctx.Done():
			return equal, nil
		}
	}
	return equal, nil
}

// LoadCheckpoint do some prepare work before check data, like adjust config and create checkpoint table
func (t *TableDiff) LoadCheckpoint(ctx context.Context) ([]*ChunkRange, error) {
	ctx1, cancel1 := context.WithTimeout(ctx, 5*dbutil.DefaultTimeout)
	defer cancel1()

	err := t.setConfigHash()
	if err != nil {
		return nil, errors.Trace(err)
	}

	err = createCheckpointTable(ctx1, t.TargetTable.Conn)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if t.UseCheckpoint {
		useCheckpoint, err := loadFromCheckPoint(ctx1, t.TargetTable.Conn, t.TargetTable.Schema, t.TargetTable.Table, t.configHash)
		if err != nil {
			return nil, errors.Trace(err)
		}

		if useCheckpoint {
			log.Info("use checkpoint to load chunks")
			chunks, err := loadChunks(ctx1, t.TargetTable.Conn, t.TargetTable.InstanceID, t.TargetTable.Schema, t.TargetTable.Table)
			if err != nil {
				log.Error("load chunks info", zap.Error(err))
				return nil, errors.Trace(err)
			}

			return chunks, nil
		}
	}

	// clean old checkpoint infomation, and initial table summary
	err = cleanCheckpoint(ctx1, t.TargetTable.Conn, t.TargetTable.Schema, t.TargetTable.Table)
	if err != nil {
		return nil, errors.Trace(err)
	}

	err = initTableSummary(ctx1, t.TargetTable.Conn, t.TargetTable.Schema, t.TargetTable.Table, t.configHash)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return nil, nil
}

func (t *TableDiff) getSourceTableChecksum(ctx context.Context, chunk *ChunkRange) (int64, error) {
	var checksum int64

	for _, sourceTable := range t.SourceTables {
		checksumTmp, err := dbutil.GetCRC32Checksum(ctx, sourceTable.Conn, sourceTable.Schema, sourceTable.Table, t.TargetTable.info, chunk.Where, utils.StringsToInterfaces(chunk.Args), utils.SliceToMap(t.IgnoreColumns))
		if err != nil {
			return -1, errors.Trace(err)
		}

		checksum ^= checksumTmp
	}
	return checksum, nil
}

func (t *TableDiff) checkChunksDataEqual(ctx context.Context, filterByRand bool, chunks chan *ChunkRange, resultCh chan bool) {
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			if chunk.State == successState || chunk.State == ignoreState {
				resultCh <- true
				continue
			}
			eq, err := t.checkChunkDataEqual(ctx, filterByRand, chunk)
			if err != nil {
				log.Error("check chunk data equal failed", zap.String("chunk", chunk.String()), zap.Error(err))
				resultCh <- false
			} else {
				if !eq {
					log.Warn("check chunk data not equal", zap.String("chunk", chunk.String()))
				}
				resultCh <- eq
			}
		case <-ctx.Done():
			return
		}
	}
}

func (t *TableDiff) checkChunkDataEqual(ctx context.Context, filterByRand bool, chunk *ChunkRange) (equal bool, err error) {
	update := func() {
		ctx1, cancel1 := context.WithTimeout(ctx, dbutil.DefaultTimeout)
		defer cancel1()

		err1 := saveChunk(ctx1, t.TargetTable.Conn, chunk.ID, t.TargetTable.InstanceID, t.TargetTable.Schema, t.TargetTable.Table, "", chunk)
		if err1 != nil {
			log.Warn("update chunk info", zap.Error(err1))
		}
	}

	defer func() {
		if chunk.State != ignoreState {
			if err != nil {
				chunk.State = errorState
			} else if equal {
				chunk.State = successState
			} else {
				chunk.State = failedState
			}
		}
		update()
	}()

	if filterByRand {
		rand.Seed(time.Now().UnixNano())
		r := rand.Intn(100)
		if r > t.Sample {
			chunk.State = ignoreState
			return true, nil
		}
	}

	chunk.State = checkingState
	update()

	if t.UseChecksum {
		// first check the checksum is equal or not
		equal, err = t.compareChecksum(ctx, chunk)
		if err != nil {
			return false, errors.Trace(err)
		}
		if equal {
			return true, nil
		}
	}

	if t.UseChecksum && t.OnlyUseChecksum {
		return false, nil
	}

	// if checksum is not equal or don't need compare checksum, compare the data
	log.Info("select data and then check data", zap.String("table", dbutil.TableName(t.TargetTable.Schema, t.TargetTable.Table)), zap.String("where", chunk.Where), zap.Reflect("args", chunk.Args))

	equal, err = t.compareRows(ctx, chunk)
	if err != nil {
		return false, errors.Trace(err)
	}

	return equal, nil
}

func (t *TableDiff) compareChecksum(ctx context.Context, chunk *ChunkRange) (bool, error) {
	// first check the checksum is equal or not
	sourceChecksum, err := t.getSourceTableChecksum(ctx, chunk)
	if err != nil {
		return false, errors.Trace(err)
	}

	targetChecksum, err := dbutil.GetCRC32Checksum(ctx, t.TargetTable.Conn, t.TargetTable.Schema, t.TargetTable.Table, t.TargetTable.info, chunk.Where, utils.StringsToInterfaces(chunk.Args), utils.SliceToMap(t.IgnoreColumns))
	if err != nil {
		return false, errors.Trace(err)
	}
	if sourceChecksum == targetChecksum {
		log.Info("checksum is equal", zap.String("table", dbutil.TableName(t.TargetTable.Schema, t.TargetTable.Table)), zap.String("where", chunk.Where), zap.Reflect("args", chunk.Args), zap.Int64("checksum", sourceChecksum))
		return true, nil
	}

	log.Warn("checksum is not equal", zap.String("table", dbutil.TableName(t.TargetTable.Schema, t.TargetTable.Table)), zap.String("where", chunk.Where), zap.Reflect("args", chunk.Args), zap.Int64("source checksum", sourceChecksum), zap.Int64("target checksum", targetChecksum))

	return false, nil
}

func (t *TableDiff) compareRows(ctx context.Context, chunk *ChunkRange) (bool, error) {
	sourceRows := make(map[string][]map[string]*dbutil.ColumnData)
	args := utils.StringsToInterfaces(chunk.Args)
	ignoreCloumns := utils.SliceToMap(t.IgnoreColumns)

	targetRows, orderKeyCols, err := getChunkRows(ctx, t.TargetTable.Conn, t.TargetTable.Schema, t.TargetTable.Table, t.TargetTable.info, chunk.Where, args, ignoreCloumns, t.Collation)
	if err != nil {
		return false, errors.Trace(err)
	}

	// judge rows have all order keys to avoid panic
	if len(targetRows) > 0 {
		if !rowContainsCols(targetRows[0], orderKeyCols) {
			return false, errors.Errorf("%s.%s.%s's data don't contain all keys %v", t.TargetTable.InstanceID, t.TargetTable.Schema, t.TargetTable.Table, orderKeyCols)
		}
	}

	for i, sourceTable := range t.SourceTables {
		rows, _, err := getChunkRows(ctx, sourceTable.Conn, sourceTable.Schema, sourceTable.Table, sourceTable.info, chunk.Where, args, ignoreCloumns, t.Collation)
		if err != nil {
			return false, errors.Trace(err)
		}

		// judge rows have all order keys to avoid panic
		if len(rows) > 0 {
			if !rowContainsCols(rows[0], orderKeyCols) {
				return false, errors.Errorf("%s.%s.%s's data don't contain all keys %v", sourceTable.InstanceID, sourceTable.Schema, sourceTable.Table, orderKeyCols)
			}
		}

		sourceRows[fmt.Sprintf("source-%d", i)] = rows
	}

	var (
		equal     = true
		rowsData1 = make([]map[string]*dbutil.ColumnData, 0, 100)
		rowsData2 = make([]map[string]*dbutil.ColumnData, 0, 100)
	)

	rowDatas := &RowDatas{
		Rows:         make([]RowData, 0, len(sourceRows)),
		OrderKeyCols: orderKeyCols,
	}
	heap.Init(rowDatas)
	sourceMap := make(map[string]interface{})
	sourceOffset := make(map[string]int)
	for {
		for source, rows := range sourceRows {
			if _, ok := sourceMap[source]; ok {
				continue
			}
			if sourceOffset[source] == len(rows) {
				delete(sourceRows, source)
				continue
			}

			data := rows[sourceOffset[source]]
			heap.Push(rowDatas, RowData{
				Data:   data,
				Source: source,
			})
			sourceMap[source] = struct{}{}
			sourceOffset[source]++
		}

		if rowDatas.Len() == 0 {
			break
		}

		rowData := heap.Pop(rowDatas).(RowData)
		rowsData1 = append(rowsData1, rowData.Data)
		delete(sourceMap, rowData.Source)
	}

	rowsData2 = targetRows

	var index1, index2 int
	for {
		if index1 == len(rowsData1) {
			// all the rowsData2's data should be deleted
			for ; index2 < len(rowsData2); index2++ {
				sql := generateDML("delete", rowsData2[index2], orderKeyCols, t.TargetTable.info, t.TargetTable.Schema)
				log.Info("[delete]", zap.String("sql", sql))
				t.wg.Add(1)
				t.sqlCh <- sql
				equal = false
			}
			break
		}
		if index2 == len(rowsData2) {
			// rowsData2 lack some data, should insert them
			for ; index1 < len(rowsData1); index1++ {
				sql := generateDML("replace", rowsData1[index1], orderKeyCols, t.TargetTable.info, t.TargetTable.Schema)
				log.Info("[insert]", zap.String("sql", sql))
				t.wg.Add(1)
				t.sqlCh <- sql
				equal = false
			}
			break
		}
		eq, cmp, err := compareData(rowsData1[index1], rowsData2[index2], orderKeyCols)
		if err != nil {
			return false, errors.Trace(err)
		}
		if eq {
			index1++
			index2++
			continue
		}
		equal = false
		switch cmp {
		case 1:
			// delete
			sql := generateDML("delete", rowsData2[index2], orderKeyCols, t.TargetTable.info, t.TargetTable.Schema)
			log.Info("[delete]", zap.String("sql", sql))
			t.wg.Add(1)
			t.sqlCh <- sql
			index2++
		case -1:
			// insert
			sql := generateDML("replace", rowsData1[index1], orderKeyCols, t.TargetTable.info, t.TargetTable.Schema)
			log.Info("[insert]", zap.String("sql", sql))
			t.wg.Add(1)
			t.sqlCh <- sql
			index1++
		case 0:
			// update
			sql := generateDML("replace", rowsData1[index1], orderKeyCols, t.TargetTable.info, t.TargetTable.Schema)
			log.Info("[update]", zap.String("sql", sql))
			t.wg.Add(1)
			t.sqlCh <- sql
			index1++
			index2++
		}
	}

	return equal, nil
}

// WriteSqls write sqls to file
func (t *TableDiff) WriteSqls(ctx context.Context, writeFixSQL func(string) error) chan bool {
	t.wg.Add(1)
	stopWriteCh := make(chan bool)

	go func() {
		defer t.wg.Done()

		stop := false
		for {
			select {
			case dml, ok := <-t.sqlCh:
				if !ok {
					return
				}

				err := writeFixSQL(fmt.Sprintf("%s\n", dml))
				if err != nil {
					log.Error("write sql failed", zap.String("sql", dml), zap.Error(err))
				}
				t.wg.Done()
			case <-stopWriteCh:
				stop = true
			case <-ctx.Done():
				return
			default:
				if stop {
					return
				}

				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	return stopWriteCh
}

func (t *TableDiff) UpdateSummaryInfo(ctx context.Context) chan bool {
	t.wg.Add(1)
	stopUpdateCh := make(chan bool)

	go func() {
		update := func() {
			ctx1, cancel1 := context.WithTimeout(ctx, dbutil.DefaultTimeout)
			defer cancel1()

			err := updateTableSummary(ctx1, t.TargetTable.Conn, t.TargetTable.InstanceID, t.TargetTable.Schema, t.TargetTable.Table)
			if err != nil {
				log.Error("save table summary info failed", zap.String("schema", t.TargetTable.Schema), zap.String("table", t.TargetTable.Table), zap.Error(err))
			}
		}
		defer func() {
			update()
			t.wg.Done()
		}()

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-stopUpdateCh:
				return
			case <-ticker.C:
				update()
			}
		}
	}()

	return stopUpdateCh
}

func generateDML(tp string, data map[string]*dbutil.ColumnData, keys []*model.ColumnInfo, table *model.TableInfo, schema string) (sql string) {
	switch tp {
	case "replace":
		colNames := make([]string, 0, len(table.Columns))
		values := make([]string, 0, len(table.Columns))
		for _, col := range table.Columns {
			colNames = append(colNames, fmt.Sprintf("`%s`", col.Name.O))
			if data[col.Name.O].IsNull {
				values = append(values, "NULL")
				continue
			}

			if needQuotes(col.FieldType) {
				values = append(values, fmt.Sprintf("'%s'", string(data[col.Name.O].Data)))
			} else {
				values = append(values, string(data[col.Name.O].Data))
			}
		}

		sql = fmt.Sprintf("REPLACE INTO `%s`.`%s`(%s) VALUES (%s);", schema, table.Name, strings.Join(colNames, ","), strings.Join(values, ","))
	case "delete":
		kvs := make([]string, 0, len(keys))
		for _, col := range keys {
			if data[col.Name.O].IsNull {
				kvs = append(kvs, fmt.Sprintf("`%s` is NULL", col.Name.O))
				continue
			}

			if needQuotes(col.FieldType) {
				kvs = append(kvs, fmt.Sprintf("`%s` = '%s'", col.Name.O, string(data[col.Name.O].Data)))
			} else {
				kvs = append(kvs, fmt.Sprintf("`%s` = %s", col.Name.O, string(data[col.Name.O].Data)))
			}
		}
		sql = fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE %s;", schema, table.Name, strings.Join(kvs, " AND "))
	default:
		log.Error("unknown sql type", zap.String("type", tp))
	}

	return
}

func compareData(map1, map2 map[string]*dbutil.ColumnData, orderKeyCols []*model.ColumnInfo) (bool, int32, error) {
	var (
		equal        = true
		data1, data2 *dbutil.ColumnData
		key          string
		ok           bool
		cmp          int32
	)

	for key, data1 = range map1 {
		if data2, ok = map2[key]; !ok {
			return false, 0, errors.Errorf("don't have key %s", key)
		}
		if (string(data1.Data) == string(data2.Data)) && (data1.IsNull == data2.IsNull) {
			continue
		}
		equal = false
		if data1.IsNull == data2.IsNull {
			log.Error("find difference data", zap.String("column", key), zap.Reflect("data1", map1), zap.Reflect("data2", map2))
		} else {
			log.Error("find difference data, one of them is NULL", zap.String("column", key), zap.Reflect("data1", map1), zap.Reflect("data2", map2))
		}
		break
	}
	if equal {
		return true, 0, nil
	}

	for _, col := range orderKeyCols {
		if data1, ok = map1[col.Name.O]; !ok {
			return false, 0, errors.Errorf("don't have key %s", col.Name.O)
		}
		if data2, ok = map2[col.Name.O]; !ok {
			return false, 0, errors.Errorf("don't have key %s", col.Name.O)
		}
		if needQuotes(col.FieldType) {
			strData1 := string(data1.Data)
			strData2 := string(data2.Data)

			if len(strData1) == len(strData2) && strData1 == strData2 {
				continue
			}

			if strData1 < strData2 {
				cmp = -1
			} else if strData1 > strData2 {
				cmp = 1
			}
			break

		} else {
			num1, err1 := strconv.ParseFloat(string(data1.Data), 64)
			num2, err2 := strconv.ParseFloat(string(data2.Data), 64)
			if err1 != nil || err2 != nil {
				return false, 0, errors.Errorf("convert %s, %s to float failed, err1: %v, err2: %v", string(data1.Data), string(data2.Data), err1, err2)
			}

			if num1 == num2 {
				continue
			}

			if num1 < num2 {
				cmp = -1
			} else if num1 > num2 {
				cmp = 1
			}
			break
		}
	}

	return false, cmp, nil
}

func getChunkRows(ctx context.Context, db *sql.DB, schema, table string, tableInfo *model.TableInfo, where string,
	args []interface{}, ignoreColumns map[string]interface{}, collation string) ([]map[string]*dbutil.ColumnData, []*model.ColumnInfo, error) {
	orderKeys, orderKeyCols := dbutil.SelectUniqueOrderKey(tableInfo)
	columns := "*"

	if len(ignoreColumns) != 0 {
		columnNames := make([]string, 0, len(tableInfo.Columns))
		for _, col := range tableInfo.Columns {
			if _, ok := ignoreColumns[col.Name.O]; ok {
				continue
			}
			columnNames = append(columnNames, col.Name.O)
		}
		columns = strings.Join(columnNames, ", ")
	}

	if orderKeys[0] == dbutil.ImplicitColName {
		columns = fmt.Sprintf("%s, %s", columns, dbutil.ImplicitColName)
	}

	if collation != "" {
		collation = fmt.Sprintf(" COLLATE \"%s\"", collation)
	}

	query := fmt.Sprintf("SELECT /*!40001 SQL_NO_CACHE */ %s FROM `%s`.`%s` WHERE %s ORDER BY %s%s",
		columns, schema, table, where, strings.Join(orderKeys, ","), collation)

	log.Debug("select data", zap.String("sql", query), zap.Reflect("args", args))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	defer rows.Close()

	datas := make([]map[string]*dbutil.ColumnData, 0, 100)
	for rows.Next() {
		data, err := dbutil.ScanRow(rows)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		datas = append(datas, data)
	}

	return datas, orderKeyCols, errors.Trace(rows.Err())
}
