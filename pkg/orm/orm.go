// Copyright 2014 beego Author. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build go1.8

// Package orm provide ORM for MySQL/PostgreSQL/sqlite
// Simple Usage
//
//	package main
//
//	import (
//		"fmt"
//		"github.com/astaxie/beego/pkg/orm"
//		_ "github.com/go-sql-driver/mysql" // import your used driver
//	)
//
//	// Model Struct
//	type User struct {
//		Id   int    `orm:"auto"`
//		Name string `orm:"size(100)"`
//	}
//
//	func init() {
//		orm.RegisterDataBase("default", "mysql", "root:root@/my_db?charset=utf8", 30)
//	}
//
//	func main() {
//		o := orm.NewOrm()
//		user := User{Name: "slene"}
//		// insert
//		id, err := o.Insert(&user)
//		// update
//		user.Name = "astaxie"
//		num, err := o.Update(&user)
//		// read one
//		u := User{Id: user.Id}
//		err = o.Read(&u)
//		// delete
//		num, err = o.Delete(&u)
//	}
//
// more docs: http://beego.me/docs/mvc/model/overview.md
package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/astaxie/beego/logs"
)

// DebugQueries define the debug
const (
	DebugQueries = iota
)

// Define common vars
var (
	Debug            = false
	DebugLog         = NewLog(os.Stdout)
	DefaultRowsLimit = -1
	DefaultRelsDepth = 2
	DefaultTimeLoc   = time.Local
	ErrTxDone        = errors.New("<TxOrmer.Commit/Rollback> transaction already done")
	ErrMultiRows     = errors.New("<QuerySeter> return multi rows")
	ErrNoRows        = errors.New("<QuerySeter> no row found")
	ErrStmtClosed    = errors.New("<QuerySeter> stmt already closed")
	ErrArgs          = errors.New("<Ormer> args error may be empty")
	ErrNotImplement  = errors.New("have not implement")
)

// Params stores the Params
type Params map[string]interface{}

// ParamsList stores paramslist
type ParamsList []interface{}

type ormBase struct {
	alias *alias
	db    dbQuerier
}

var _ DQL = new(ormBase)
var _ DML = new(ormBase)

// get model info and model reflect value
func (o *ormBase) getMiInd(md interface{}, needPtr bool) (mi *modelInfo, ind reflect.Value) {
	val := reflect.ValueOf(md)
	ind = reflect.Indirect(val)
	typ := ind.Type()
	if needPtr && val.Kind() != reflect.Ptr {
		panic(fmt.Errorf("<Ormer> cannot use non-ptr model struct `%s`", getFullName(typ)))
	}
	name := getFullName(typ)
	if mi, ok := modelCache.getByFullName(name); ok {
		return mi, ind
	}
	panic(fmt.Errorf("<Ormer> table: `%s` not found, make sure it was registered with `RegisterModel()`", name))
}

// get field info from model info by given field name
func (o *ormBase) getFieldInfo(mi *modelInfo, name string) *fieldInfo {
	fi, ok := mi.fields.GetByAny(name)
	if !ok {
		panic(fmt.Errorf("<Ormer> cannot find field `%s` for model `%s`", name, mi.fullName))
	}
	return fi
}

// read data to model
func (o *ormBase) Read(md interface{}, cols ...string) error {
	return o.ReadWithCtx(context.Background(), md, cols...)
}
func (o *ormBase) ReadWithCtx(ctx context.Context, md interface{}, cols ...string) error {
	mi, ind := o.getMiInd(md, true)
	return o.alias.DbBaser.Read(o.db, mi, ind, o.alias.TZ, cols, false)
}

// read data to model, like Read(), but use "SELECT FOR UPDATE" form
func (o *ormBase) ReadForUpdate(md interface{}, cols ...string) error {
	return o.ReadForUpdateWithCtx(context.Background(), md, cols...)
}
func (o *ormBase) ReadForUpdateWithCtx(ctx context.Context, md interface{}, cols ...string) error {
	mi, ind := o.getMiInd(md, true)
	return o.alias.DbBaser.Read(o.db, mi, ind, o.alias.TZ, cols, true)
}

// Try to read a row from the database, or insert one if it doesn't exist
func (o *ormBase) ReadOrCreate(md interface{}, col1 string, cols ...string) (bool, int64, error) {
	return o.ReadOrCreateWithCtx(context.Background(), md, col1, cols...)
}
func (o *ormBase) ReadOrCreateWithCtx(ctx context.Context, md interface{}, col1 string, cols ...string) (bool, int64, error) {
	cols = append([]string{col1}, cols...)
	mi, ind := o.getMiInd(md, true)
	err := o.alias.DbBaser.Read(o.db, mi, ind, o.alias.TZ, cols, false)
	if err == ErrNoRows {
		// Create
		id, err := o.InsertWithCtx(ctx, md)
		return err == nil, id, err
	}

	id, vid := int64(0), ind.FieldByIndex(mi.fields.pk.fieldIndex)
	if mi.fields.pk.fieldType&IsPositiveIntegerField > 0 {
		id = int64(vid.Uint())
	} else if mi.fields.pk.rel {
		return o.ReadOrCreateWithCtx(ctx, vid.Interface(), mi.fields.pk.relModelInfo.fields.pk.name)
	} else {
		id = vid.Int()
	}

	return false, id, err
}

// insert model data to database
func (o *ormBase) Insert(md interface{}) (int64, error) {
	return o.InsertWithCtx(context.Background(), md)
}
func (o *ormBase) InsertWithCtx(ctx context.Context, md interface{}) (int64, error) {
	mi, ind := o.getMiInd(md, true)
	id, err := o.alias.DbBaser.Insert(o.db, mi, ind, o.alias.TZ)
	if err != nil {
		return id, err
	}

	o.setPk(mi, ind, id)

	return id, nil
}

// set auto pk field
func (o *ormBase) setPk(mi *modelInfo, ind reflect.Value, id int64) {
	if mi.fields.pk.auto {
		if mi.fields.pk.fieldType&IsPositiveIntegerField > 0 {
			ind.FieldByIndex(mi.fields.pk.fieldIndex).SetUint(uint64(id))
		} else {
			ind.FieldByIndex(mi.fields.pk.fieldIndex).SetInt(id)
		}
	}
}

// insert some models to database
func (o *ormBase) InsertMulti(bulk int, mds interface{}) (int64, error) {
	return o.InsertMultiWithCtx(context.Background(), bulk, mds)
}
func (o *ormBase) InsertMultiWithCtx(ctx context.Context, bulk int, mds interface{}) (int64, error) {
	var cnt int64

	sind := reflect.Indirect(reflect.ValueOf(mds))

	switch sind.Kind() {
	case reflect.Array, reflect.Slice:
		if sind.Len() == 0 {
			return cnt, ErrArgs
		}
	default:
		return cnt, ErrArgs
	}

	if bulk <= 1 {
		for i := 0; i < sind.Len(); i++ {
			ind := reflect.Indirect(sind.Index(i))
			mi, _ := o.getMiInd(ind.Interface(), false)
			id, err := o.alias.DbBaser.Insert(o.db, mi, ind, o.alias.TZ)
			if err != nil {
				return cnt, err
			}

			o.setPk(mi, ind, id)

			cnt++
		}
	} else {
		mi, _ := o.getMiInd(sind.Index(0).Interface(), false)
		return o.alias.DbBaser.InsertMulti(o.db, mi, sind, bulk, o.alias.TZ)
	}
	return cnt, nil
}

// InsertOrUpdate data to database
func (o *ormBase) InsertOrUpdate(md interface{}, colConflictAndArgs ...string) (int64, error) {
	return o.InsertOrUpdateWithCtx(context.Background(), md, colConflictAndArgs...)
}
func (o *ormBase) InsertOrUpdateWithCtx(ctx context.Context, md interface{}, colConflitAndArgs ...string) (int64, error) {
	mi, ind := o.getMiInd(md, true)
	id, err := o.alias.DbBaser.InsertOrUpdate(o.db, mi, ind, o.alias, colConflitAndArgs...)
	if err != nil {
		return id, err
	}

	o.setPk(mi, ind, id)

	return id, nil
}

// update model to database.
// cols set the columns those want to update.
func (o *ormBase) Update(md interface{}, cols ...string) (int64, error) {
	return o.UpdateWithCtx(context.Background(), md, cols...)
}
func (o *ormBase) UpdateWithCtx(ctx context.Context, md interface{}, cols ...string) (int64, error) {
	mi, ind := o.getMiInd(md, true)
	return o.alias.DbBaser.Update(o.db, mi, ind, o.alias.TZ, cols)
}

// delete model in database
// cols shows the delete conditions values read from. default is pk
func (o *ormBase) Delete(md interface{}, cols ...string) (int64, error) {
	return o.DeleteWithCtx(context.Background(), md, cols...)
}
func (o *ormBase) DeleteWithCtx(ctx context.Context, md interface{}, cols ...string) (int64, error) {
	mi, ind := o.getMiInd(md, true)
	num, err := o.alias.DbBaser.Delete(o.db, mi, ind, o.alias.TZ, cols)
	if err != nil {
		return num, err
	}
	if num > 0 {
		o.setPk(mi, ind, 0)
	}
	return num, nil
}

// create a models to models queryer
func (o *ormBase) QueryM2M(md interface{}, name string) QueryM2Mer {
	return o.QueryM2MWithCtx(context.Background(), md, name)
}
func (o *ormBase) QueryM2MWithCtx(ctx context.Context, md interface{}, name string) QueryM2Mer {
	mi, ind := o.getMiInd(md, true)
	fi := o.getFieldInfo(mi, name)

	switch {
	case fi.fieldType == RelManyToMany:
	case fi.fieldType == RelReverseMany && fi.reverseFieldInfo.mi.isThrough:
	default:
		panic(fmt.Errorf("<Ormer.QueryM2M> model `%s` . name `%s` is not a m2m field", fi.name, mi.fullName))
	}

	return newQueryM2M(md, o, mi, fi, ind)
}

// load related models to md model.
// args are limit, offset int and order string.
//
// example:
// 	orm.LoadRelated(post,"Tags")
// 	for _,tag := range post.Tags{...}
//
// make sure the relation is defined in model struct tags.
func (o *ormBase) LoadRelated(md interface{}, name string, args ...interface{}) (int64, error) {
	return o.LoadRelatedWithCtx(context.Background(), md, name, args...)
}
func (o *ormBase) LoadRelatedWithCtx(ctx context.Context, md interface{}, name string, args ...interface{}) (int64, error) {
	_, fi, ind, qseter := o.queryRelated(md, name)

	qs := qseter.(*querySet)

	var relDepth int
	var limit, offset int64
	var order string
	for i, arg := range args {
		switch i {
		case 0:
			if v, ok := arg.(bool); ok {
				if v {
					relDepth = DefaultRelsDepth
				}
			} else if v, ok := arg.(int); ok {
				relDepth = v
			}
		case 1:
			limit = ToInt64(arg)
		case 2:
			offset = ToInt64(arg)
		case 3:
			order, _ = arg.(string)
		}
	}

	switch fi.fieldType {
	case RelOneToOne, RelForeignKey, RelReverseOne:
		limit = 1
		offset = 0
	}

	qs.limit = limit
	qs.offset = offset
	qs.relDepth = relDepth

	if len(order) > 0 {
		qs.orders = []string{order}
	}

	find := ind.FieldByIndex(fi.fieldIndex)

	var nums int64
	var err error
	switch fi.fieldType {
	case RelOneToOne, RelForeignKey, RelReverseOne:
		val := reflect.New(find.Type().Elem())
		container := val.Interface()
		err = qs.One(container)
		if err == nil {
			find.Set(val)
			nums = 1
		}
	default:
		nums, err = qs.All(find.Addr().Interface())
	}

	return nums, err
}

// return a QuerySeter for related models to md model.
// it can do all, update, delete in QuerySeter.
// example:
// 	qs := orm.QueryRelated(post,"Tag")
//  qs.All(&[]*Tag{})
//
func (o *ormBase) QueryRelated(md interface{}, name string) QuerySeter {
	return o.QueryRelatedWithCtx(context.Background(), md, name)
}
func (o *ormBase) QueryRelatedWithCtx(ctx context.Context, md interface{}, name string) QuerySeter {
	// is this api needed ?
	_, _, _, qs := o.queryRelated(md, name)
	return qs
}

// get QuerySeter for related models to md model
func (o *ormBase) queryRelated(md interface{}, name string) (*modelInfo, *fieldInfo, reflect.Value, QuerySeter) {
	mi, ind := o.getMiInd(md, true)
	fi := o.getFieldInfo(mi, name)

	_, _, exist := getExistPk(mi, ind)
	if !exist {
		panic(ErrMissPK)
	}

	var qs *querySet

	switch fi.fieldType {
	case RelOneToOne, RelForeignKey, RelManyToMany:
		if !fi.inModel {
			break
		}
		qs = o.getRelQs(md, mi, fi)
	case RelReverseOne, RelReverseMany:
		if !fi.inModel {
			break
		}
		qs = o.getReverseQs(md, mi, fi)
	}

	if qs == nil {
		panic(fmt.Errorf("<Ormer> name `%s` for model `%s` is not an available rel/reverse field", md, name))
	}

	return mi, fi, ind, qs
}

// get reverse relation QuerySeter
func (o *ormBase) getReverseQs(md interface{}, mi *modelInfo, fi *fieldInfo) *querySet {
	switch fi.fieldType {
	case RelReverseOne, RelReverseMany:
	default:
		panic(fmt.Errorf("<Ormer> name `%s` for model `%s` is not an available reverse field", fi.name, mi.fullName))
	}

	var q *querySet

	if fi.fieldType == RelReverseMany && fi.reverseFieldInfo.mi.isThrough {
		q = newQuerySet(o, fi.relModelInfo).(*querySet)
		q.cond = NewCondition().And(fi.reverseFieldInfoM2M.column+ExprSep+fi.reverseFieldInfo.column, md)
	} else {
		q = newQuerySet(o, fi.reverseFieldInfo.mi).(*querySet)
		q.cond = NewCondition().And(fi.reverseFieldInfo.column, md)
	}

	return q
}

// get relation QuerySeter
func (o *ormBase) getRelQs(md interface{}, mi *modelInfo, fi *fieldInfo) *querySet {
	switch fi.fieldType {
	case RelOneToOne, RelForeignKey, RelManyToMany:
	default:
		panic(fmt.Errorf("<Ormer> name `%s` for model `%s` is not an available rel field", fi.name, mi.fullName))
	}

	q := newQuerySet(o, fi.relModelInfo).(*querySet)
	q.cond = NewCondition()

	if fi.fieldType == RelManyToMany {
		q.cond = q.cond.And(fi.reverseFieldInfoM2M.column+ExprSep+fi.reverseFieldInfo.column, md)
	} else {
		q.cond = q.cond.And(fi.reverseFieldInfo.column, md)
	}

	return q
}

// return a QuerySeter for table operations.
// table name can be string or struct.
// e.g. QueryTable("user"), QueryTable(&user{}) or QueryTable((*User)(nil)),
func (o *ormBase) QueryTable(ptrStructOrTableName interface{}) (qs QuerySeter) {
	return o.QueryTableWithCtx(context.Background(), ptrStructOrTableName)
}
func (o *ormBase) QueryTableWithCtx(ctx context.Context, ptrStructOrTableName interface{}) (qs QuerySeter) {
	var name string
	if table, ok := ptrStructOrTableName.(string); ok {
		name = nameStrategyMap[defaultNameStrategy](table)
		if mi, ok := modelCache.get(name); ok {
			qs = newQuerySet(o, mi)
		}
	} else {
		name = getFullName(indirectType(reflect.TypeOf(ptrStructOrTableName)))
		if mi, ok := modelCache.getByFullName(name); ok {
			qs = newQuerySet(o, mi)
		}
	}
	if qs == nil {
		panic(fmt.Errorf("<Ormer.QueryTable> table name: `%s` not exists", name))
	}
	return
}

// return a raw query seter for raw sql string.
func (o *ormBase) Raw(query string, args ...interface{}) RawSeter {
	return o.RawWithCtx(context.Background(), query, args...)
}
func (o *ormBase) RawWithCtx(ctx context.Context, query string, args ...interface{}) RawSeter {
	return newRawSet(o, query, args)
}

// return current using database Driver
func (o *ormBase) Driver() Driver {
	return driver(o.alias.Name)
}

// return sql.DBStats for current database
func (o *ormBase) DBStats() *sql.DBStats {
	if o.alias != nil && o.alias.DB != nil {
		stats := o.alias.DB.DB.Stats()
		return &stats
	}
	return nil
}

type orm struct {
	ormBase
}

var _ Ormer = new(orm)

func (o *orm) Begin() (TxOrmer, error) {
	return o.BeginWithCtx(context.Background())
}

func (o *orm) BeginWithCtx(ctx context.Context) (TxOrmer, error) {
	return o.BeginWithCtxAndOpts(ctx, nil)
}

func (o *orm) BeginWithOpts(opts *sql.TxOptions) (TxOrmer, error) {
	return o.BeginWithCtxAndOpts(context.Background(), opts)
}

func (o *orm) BeginWithCtxAndOpts(ctx context.Context, opts *sql.TxOptions) (TxOrmer, error) {
	tx, err := o.db.(txer).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}

	_txOrm := &txOrm{
		ormBase: ormBase{
			alias: o.alias,
			db:    &TxDB{tx: tx},
		},
	}

	var taskTxOrm TxOrmer = _txOrm
	return taskTxOrm, nil
}

func (o *orm) DoTx(task func(txOrm TxOrmer) error) error {
	return o.DoTxWithCtx(context.Background(), task)
}

func (o *orm) DoTxWithCtx(ctx context.Context, task func(txOrm TxOrmer) error) error {
	return o.DoTxWithCtxAndOpts(ctx, nil, task)
}

func (o *orm) DoTxWithOpts(opts *sql.TxOptions, task func(txOrm TxOrmer) error) error {
	return o.DoTxWithCtxAndOpts(context.Background(), opts, task)
}

func (o *orm) DoTxWithCtxAndOpts(ctx context.Context, opts *sql.TxOptions, task func(txOrm TxOrmer) error) error {
	_txOrm, err := o.BeginWithCtxAndOpts(ctx, opts)
	if err != nil {
		return err
	}
	panicked := true
	defer func() {
		if panicked || err != nil {
			e := _txOrm.Rollback()
			if e != nil {
				logs.Error("rollback transaction failed: %v,%v", e, panicked)
			}
		} else {
			e := _txOrm.Commit()
			if e != nil {
				logs.Error("commit transaction failed: %v,%v", e, panicked)
			}
		}
	}()

	var taskTxOrm = _txOrm
	err = task(taskTxOrm)
	panicked = false
	return err
}

type txOrm struct {
	ormBase
}

var _ TxOrmer = new(txOrm)

func (t *txOrm) Commit() error {
	return t.db.(txEnder).Commit()
}

func (t *txOrm) Rollback() error {
	return t.db.(txEnder).Rollback()
}

// NewOrm create new orm
func NewOrm() Ormer {
	BootStrap() // execute only once
	return NewOrmUsingDB(`default`)
}

// NewOrm create new orm with the name
func NewOrmUsingDB(aliasName string) Ormer {
	o := new(orm)
	if al, ok := dataBaseCache.get(aliasName); ok {
		o.alias = al
		if Debug {
			o.db = newDbQueryLog(al, al.DB)
		} else {
			o.db = al.DB
		}
	} else {
		panic(fmt.Errorf("<Ormer.Using> unknown db alias name `%s`", aliasName))
	}
	return o
}

// NewOrmWithDB create a new ormer object with specify *sql.DB for query
func NewOrmWithDB(driverName, aliasName string, db *sql.DB) (Ormer, error) {
	var al *alias

	if dr, ok := drivers[driverName]; ok {
		al = new(alias)
		al.DbBaser = dbBasers[dr]
		al.Driver = dr
	} else {
		return nil, fmt.Errorf("driver name `%s` have not registered", driverName)
	}

	al.Name = aliasName
	al.DriverName = driverName
	al.DB = &DB{
		RWMutex:        new(sync.RWMutex),
		DB:             db,
		stmtDecorators: newStmtDecoratorLruWithEvict(),
	}

	detectTZ(al)

	o := new(orm)
	o.alias = al

	if Debug {
		o.db = newDbQueryLog(o.alias, db)
	} else {
		o.db = db
	}

	return o, nil
}
