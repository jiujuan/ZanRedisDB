package rockredis

import (
	"encoding/binary"
	"errors"
	"time"

	ps "github.com/prometheus/client_golang/prometheus"
	"github.com/youzan/ZanRedisDB/common"
	"github.com/youzan/ZanRedisDB/engine"
	"github.com/youzan/ZanRedisDB/metric"
	"github.com/youzan/ZanRedisDB/slow"
	"github.com/youzan/gorocksdb"
)

// we can use ring buffer to allow the list pop and push many times
// when the tail reach the end we roll to the start and check if full.
// Note: to clean the huge list, we can set some meta for each list,
// such as max elements or the max keep time, while insert we auto clean
// the data old than the meta (by number or by keep time)
const (
	listHeadSeq int64 = 1
	listTailSeq int64 = 2

	listMinSeq     int64 = 1000
	listMaxSeq     int64 = 1<<62 - 1000
	listInitialSeq int64 = listMinSeq + (listMaxSeq-listMinSeq)/2
)

var errLMetaKey = errors.New("invalid lmeta key")
var errListKey = errors.New("invalid list key")
var errListSeq = errors.New("invalid list sequence, overflow")
var errListIndex = errors.New("invalid list index")
var errListMeta = errors.New("invalid list meta data")

func lEncodeMetaKey(key []byte) []byte {
	buf := make([]byte, len(key)+1+len(metaPrefix))
	pos := 0
	buf[pos] = LMetaType
	pos++
	copy(buf[pos:], metaPrefix)
	pos += len(metaPrefix)

	copy(buf[pos:], key)
	return buf
}

func lDecodeMetaKey(ek []byte) ([]byte, error) {
	pos := 0
	if pos+1+len(metaPrefix) > len(ek) || ek[pos] != LMetaType {
		return nil, errLMetaKey
	}

	pos++
	pos += len(metaPrefix)
	return ek[pos:], nil
}

func lEncodeMinKey() []byte {
	return lEncodeMetaKey(nil)
}

func lEncodeMaxKey() []byte {
	ek := lEncodeMetaKey(nil)
	ek[len(ek)-1] = ek[len(ek)-1] + 1
	return ek
}

func lEncodeListKey(table []byte, key []byte, seq int64) []byte {
	buf := make([]byte, getDataTablePrefixBufLen(ListType, table)+len(key)+2+8)

	pos := encodeDataTablePrefixToBuf(buf, ListType, table)

	binary.BigEndian.PutUint16(buf[pos:], uint16(len(key)))
	pos += 2

	copy(buf[pos:], key)
	pos += len(key)

	binary.BigEndian.PutUint64(buf[pos:], uint64(seq))

	return buf
}

func lDecodeListKey(ek []byte) (table []byte, key []byte, seq int64, err error) {
	table, pos, derr := decodeDataTablePrefixFromBuf(ek, ListType)
	if derr != nil {
		err = derr
		return
	}

	if pos+2 > len(ek) {
		err = errListKey
		return
	}

	keyLen := int(binary.BigEndian.Uint16(ek[pos:]))
	pos += 2
	if keyLen+pos+8 != len(ek) {
		err = errListKey
		return
	}

	key = ek[pos : pos+keyLen]
	seq = int64(binary.BigEndian.Uint64(ek[pos+keyLen:]))
	return
}

func (db *RockDB) fixListKey(ts int64, key []byte) {
	// fix head and tail by iterator to find if any list key found or not found
	var headSeq int64
	var tailSeq int64
	var llen int64

	keyInfo, headSeq, tailSeq, llen, _, err := db.lHeaderAndMeta(ts, key, false)
	if err != nil {
		dbLog.Warningf("read list %v meta error: %v", string(key), err.Error())
		return
	}
	if keyInfo.IsNotExistOrExpired() {
		return
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey
	defer db.wb.Clear()
	dbLog.Infof("list %v before fix: meta: %v, %v", string(key), headSeq, tailSeq)
	startKey := lEncodeListKey(table, rk, listMinSeq)
	stopKey := lEncodeListKey(table, rk, listMaxSeq)
	rit, err := engine.NewDBRangeIterator(db.eng, startKey, stopKey, common.RangeClose, false)
	if err != nil {
		dbLog.Warningf("read list %v error: %v", string(key), err.Error())
		return
	}
	defer rit.Close()
	var fixedHead int64
	var fixedTail int64
	var cnt int64
	lastSeq := int64(-1)
	for ; rit.Valid(); rit.Next() {
		_, _, seq, err := lDecodeListKey(rit.RefKey())
		if err != nil {
			dbLog.Warningf("decode list %v error: %v", rit.Key(), err.Error())
			return
		}
		cnt++
		if lastSeq < 0 {
			fixedHead = seq
		} else if lastSeq+1 != seq {
			dbLog.Warningf("list %v should be continuous: last %v, cur: %v", string(key),
				lastSeq, seq)
			return
		}

		lastSeq = seq
		fixedTail = seq
	}
	if headSeq == fixedHead && tailSeq == fixedTail {
		dbLog.Infof("list %v no need to fix %v, %v", string(key), fixedHead, fixedTail)
		return
	}
	if llen == 0 && cnt == 0 {
		dbLog.Infof("list %v no need to fix since empty", string(key))
		return
	}
	if cnt == 0 {
		metaKey := lEncodeMetaKey(key)
		db.wb.Delete(metaKey)
		db.IncrTableKeyCount(table, -1, db.wb)
	} else {
		_, err = db.lSetMeta(key, keyInfo.OldHeader, fixedHead, fixedTail, ts, db.wb)
		if err != nil {
			return
		}
	}
	dbLog.Infof("list %v fixed to %v, %v, cnt: %v", string(key), fixedHead, fixedTail, cnt)
	db.eng.Write(db.defaultWriteOpts, db.wb)
}

func (db *RockDB) lpush(ts int64, key []byte, whereSeq int64, args ...[]byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	wb := db.wb
	defer wb.Clear()
	keyInfo, err := db.prepareCollKeyForWrite(ts, ListType, key, nil)
	if err != nil {
		return 0, err
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey

	headSeq, tailSeq, size, _, err := parseListMeta(keyInfo.MetaData())
	if err != nil {
		return 0, err
	}
	if dbLog.Level() >= common.LOG_DETAIL {
		dbLog.Debugf("lpush %v list %v meta : %v, %v, %v", whereSeq, string(key), headSeq, tailSeq, size)
	}

	pushCnt := len(args)
	if pushCnt == 0 {
		return int64(size), nil
	}

	seq := headSeq
	var delta int64 = -1
	if whereSeq == listTailSeq {
		seq = tailSeq
		delta = 1
	}

	//	append elements
	if size > 0 {
		seq += delta
	}

	checkSeq := seq + int64(pushCnt-1)*delta
	if checkSeq <= listMinSeq || checkSeq >= listMaxSeq {
		return 0, errListSeq
	}
	for i := 0; i < pushCnt; i++ {
		ek := lEncodeListKey(table, rk, seq+int64(i)*delta)
		// we assume there is no bug, so it must not override
		v, _ := db.eng.GetBytesNoLock(db.defaultReadOpts, ek)
		if v != nil {
			dbLog.Warningf("list %v should not override the old value: %v, meta: %v, %v,%v", string(key),
				v, seq, headSeq, tailSeq)
			db.fixListKey(ts, key)
			return 0, errListSeq
		}
		wb.Put(ek, args[i])
	}
	// rewrite old expired value should keep table counter unchanged
	if size == 0 && pushCnt > 0 && !keyInfo.Expired {
		db.IncrTableKeyCount(table, 1, wb)
	}
	seq += int64(pushCnt-1) * delta
	//	set meta info
	if whereSeq == listHeadSeq {
		headSeq = seq
	} else {
		tailSeq = seq
	}

	_, err = db.lSetMeta(key, keyInfo.OldHeader, headSeq, tailSeq, ts, wb)
	if dbLog.Level() >= common.LOG_DETAIL {
		dbLog.Debugf("lpush %v list %v meta updated to: %v, %v", whereSeq,
			string(key), headSeq, tailSeq)
	}
	if err != nil {
		db.fixListKey(ts, key)
		return 0, err
	}
	err = db.eng.Write(db.defaultWriteOpts, wb)

	newNum := int64(size) + int64(pushCnt)
	db.topLargeCollKeys.Update(key, int(newNum))
	slow.LogLargeCollection(int(newNum), slow.NewSlowLogInfo(string(table), string(key), "list"))
	if newNum > collectionLengthForMetric {
		metric.CollectionLenDist.With(ps.Labels{
			"table": string(table),
		}).Observe(float64(newNum))
	}
	return newNum, err
}

func (db *RockDB) lpop(ts int64, key []byte, whereSeq int64) ([]byte, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}

	keyInfo, headSeq, tailSeq, size, _, err := db.lHeaderAndMeta(ts, key, false)
	if err != nil {
		return nil, err
	}
	if keyInfo.IsNotExistOrExpired() {
		return nil, nil
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey

	if size == 0 {
		return nil, nil
	}
	if dbLog.Level() >= common.LOG_DETAIL {
		dbLog.Debugf("pop %v list %v meta: %v, %v", whereSeq, string(key), headSeq, tailSeq)
	}

	wb := db.wb
	defer wb.Clear()
	var value []byte

	var seq int64 = headSeq
	if whereSeq == listTailSeq {
		seq = tailSeq
	}

	itemKey := lEncodeListKey(table, rk, seq)
	value, err = db.eng.GetBytesNoLock(db.defaultReadOpts, itemKey)
	// nil value means not exist
	// empty value should be ""
	// since we pop should success if size is not zero, we need fix this
	if err != nil || value == nil {
		dbLog.Warningf("list %v pop error: %v, meta: %v, %v, %v", string(key), err,
			seq, headSeq, tailSeq)
		db.fixListKey(ts, key)
		return nil, err
	}

	if whereSeq == listHeadSeq {
		headSeq += 1
	} else {
		tailSeq -= 1
	}

	wb.Delete(itemKey)
	newNum, err := db.lSetMeta(key, keyInfo.OldHeader, headSeq, tailSeq, ts, wb)
	if dbLog.Level() >= common.LOG_DETAIL {
		dbLog.Debugf("pop %v list %v meta updated to: %v, %v, %v", whereSeq, string(key), headSeq, tailSeq, newNum)
	}
	if err != nil {
		db.fixListKey(ts, key)
		return nil, err
	}
	if newNum == 0 {
		// list is empty after delete
		db.IncrTableKeyCount(table, -1, wb)
		//delete the expire data related to the list key
		db.delExpire(ListType, key, nil, false, wb)
	}
	db.topLargeCollKeys.Update(key, int(newNum))
	err = db.eng.Write(db.defaultWriteOpts, wb)
	return value, err
}

func (db *RockDB) ltrim2(ts int64, key []byte, startP, stopP int64) error {
	if err := checkKeySize(key); err != nil {
		return err
	}

	keyInfo, headSeq, _, llen, _, err := db.lHeaderAndMeta(ts, key, false)
	if err != nil {
		return err
	}
	if keyInfo.IsNotExistOrExpired() {
		return nil
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey
	wb := db.wb
	defer wb.Clear()

	start := int64(startP)
	stop := int64(stopP)

	if start < 0 {
		start = llen + start
	}
	if stop < 0 {
		stop = llen + stop
	}
	newLen := int64(0)
	// whole list deleted
	if start >= llen || start > stop {
		db.lDelete(ts, key, db.wb)
	} else {
		if start < 0 {
			start = 0
		}
		if stop >= llen {
			stop = llen - 1
		}

		if start > 0 {
			if start > RangeDeleteNum {
				wb.DeleteRange(lEncodeListKey(table, rk, headSeq), lEncodeListKey(table, rk, headSeq+start))
			} else {
				for i := int64(0); i < start; i++ {
					wb.Delete(lEncodeListKey(table, rk, headSeq+i))
				}
			}
		}
		if stop < int64(llen-1) {
			if llen-stop > RangeDeleteNum {
				wb.DeleteRange(lEncodeListKey(table, rk, headSeq+int64(stop+1)),
					lEncodeListKey(table, rk, headSeq+llen))
			} else {
				for i := int64(stop + 1); i < llen; i++ {
					wb.Delete(lEncodeListKey(table, rk, headSeq+i))
				}
			}
		}

		newLen, err = db.lSetMeta(key, keyInfo.OldHeader, headSeq+start, headSeq+stop, ts, wb)
		if err != nil {
			db.fixListKey(ts, key)
			return err
		}
	}
	if llen > 0 && newLen == 0 {
		db.IncrTableKeyCount(table, -1, wb)
	}
	if newLen == 0 {
		//delete the expire data related to the list key
		db.delExpire(ListType, key, nil, false, wb)
	}

	db.topLargeCollKeys.Update(key, int(newLen))
	return db.eng.Write(db.defaultWriteOpts, wb)
}

func (db *RockDB) ltrim(ts int64, key []byte, trimSize, whereSeq int64) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}

	if trimSize == 0 {
		return 0, nil
	}

	keyInfo, headSeq, tailSeq, size, _, err := db.lHeaderAndMeta(ts, key, false)
	if err != nil {
		return 0, err
	}
	if keyInfo.IsNotExistOrExpired() {
		return 0, nil
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey

	if size == 0 {
		return 0, nil
	}

	var (
		trimStartSeq int64
		trimEndSeq   int64
	)

	if whereSeq == listHeadSeq {
		trimStartSeq = headSeq
		trimEndSeq = MinInt64(trimStartSeq+trimSize-1, tailSeq)
		headSeq = trimEndSeq + 1
	} else {
		trimEndSeq = tailSeq
		trimStartSeq = MaxInt64(trimEndSeq-trimSize+1, headSeq)
		tailSeq = trimStartSeq - 1
	}

	wb := db.wb
	defer wb.Clear()
	if trimEndSeq-trimStartSeq > RangeDeleteNum {
		itemStartKey := lEncodeListKey(table, rk, trimStartSeq)
		itemEndKey := lEncodeListKey(table, rk, trimEndSeq)
		wb.DeleteRange(itemStartKey, itemEndKey)
		wb.Delete(itemEndKey)
	} else {
		for trimSeq := trimStartSeq; trimSeq <= trimEndSeq; trimSeq++ {
			itemKey := lEncodeListKey(table, rk, trimSeq)
			wb.Delete(itemKey)
		}
	}

	newLen, err := db.lSetMeta(key, keyInfo.OldHeader, headSeq, tailSeq, ts, wb)
	if err != nil {
		db.fixListKey(ts, key)
		return 0, err
	}
	if newLen == 0 {
		// list is empty after trim
		db.IncrTableKeyCount(table, -1, wb)
		//delete the expire data related to the list key
		db.delExpire(ListType, key, nil, false, wb)
	}

	db.topLargeCollKeys.Update(key, int(newLen))
	err = db.eng.Write(db.defaultWriteOpts, wb)
	return trimEndSeq - trimStartSeq + 1, err
}

//	ps : here just focus on deleting the list data,
//		 any other likes expire is ignore.
func (db *RockDB) lDelete(ts int64, key []byte, wb *gorocksdb.WriteBatch) int64 {
	keyInfo, headSeq, tailSeq, size, _, err := db.lHeaderAndMeta(ts, key, false)
	if err != nil {
		return 0
	}
	// no need delete if expired
	if keyInfo.IsNotExistOrExpired() || size == 0 {
		return 0
	}

	table := keyInfo.Table
	mk := lEncodeMetaKey(key)
	wb.Delete(mk)
	if size > 0 {
		db.IncrTableKeyCount(table, -1, wb)
	}
	db.topLargeCollKeys.Update(key, int(0))
	if db.cfg.ExpirationPolicy == common.WaitCompact {
		// for compact ttl , we can just delete the meta
		return size
	}
	rk := keyInfo.VerKey

	startKey := lEncodeListKey(table, rk, headSeq)
	stopKey := lEncodeListKey(table, rk, tailSeq)

	if size > RangeDeleteNum {
		wb.DeleteRange(startKey, stopKey)
	} else {
		opts := engine.IteratorOpts{
			Range:     engine.Range{Min: startKey, Max: stopKey, Type: common.RangeClose},
			Reverse:   false,
			IgnoreDel: true,
		}
		rit, err := engine.NewDBRangeIteratorWithOpts(db.eng, opts)
		if err != nil {
			return 0
		}
		for ; rit.Valid(); rit.Next() {
			wb.Delete(rit.RefKey())
		}
		rit.Close()
	}
	// delete range is [left, right), so we need delete end

	wb.Delete(stopKey)
	return size
}

func parseListMeta(v []byte) (headSeq int64, tailSeq int64, size int64, ts int64, err error) {
	if len(v) == 0 {
		headSeq = listInitialSeq
		tailSeq = listInitialSeq
		size = 0
		return
	} else {
		if len(v) < 16 {
			err = errListMeta
			return
		}
		headSeq = int64(binary.BigEndian.Uint64(v[0:8]))
		tailSeq = int64(binary.BigEndian.Uint64(v[8:16]))
		if len(v) >= 24 {
			ts = int64(binary.BigEndian.Uint64(v[16 : 16+8]))
		}
		size = tailSeq - headSeq + 1
	}
	return
}

func encodeListMeta(oldh *headerMetaValue, headSeq int64, tailSeq int64, ts int64) []byte {
	buf := make([]byte, 24)
	binary.BigEndian.PutUint64(buf[0:8], uint64(headSeq))
	binary.BigEndian.PutUint64(buf[8:16], uint64(tailSeq))
	binary.BigEndian.PutUint64(buf[16:24], uint64(ts))
	oldh.UserData = buf
	nv := oldh.encodeWithData()
	return nv
}

func (db *RockDB) lSetMeta(key []byte, oldh *headerMetaValue, headSeq int64, tailSeq int64, ts int64, wb *gorocksdb.WriteBatch) (int64, error) {
	metaKey := lEncodeMetaKey(key)
	size := tailSeq - headSeq + 1
	if size < 0 {
		dbLog.Warningf("list %v invalid meta sequence range [%d, %d]", string(key), headSeq, tailSeq)
		return 0, errListSeq
	} else if size == 0 {
		wb.Delete(metaKey)
	} else {
		buf := encodeListMeta(oldh, headSeq, tailSeq, ts)
		wb.Put(metaKey, buf)
	}
	return size, nil
}

func (db *RockDB) lHeaderAndMeta(ts int64, key []byte, useLock bool) (collVerKeyInfo, int64, int64, int64, int64, error) {
	keyInfo, err := db.GetCollVersionKey(ts, ListType, key, useLock)
	if err != nil {
		return keyInfo, 0, 0, 0, 0, err
	}
	headSeq, tailSeq, size, ts, err := parseListMeta(keyInfo.MetaData())
	return keyInfo, headSeq, tailSeq, size, ts, err
}

func (db *RockDB) LIndex(key []byte, index int64) ([]byte, error) {
	ts := time.Now().UnixNano()
	keyInfo, headSeq, tailSeq, _, _, err := db.lHeaderAndMeta(ts, key, true)
	if err != nil {
		return nil, err
	}
	if keyInfo.IsNotExistOrExpired() {
		return nil, nil
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey

	var seq int64
	if index >= 0 {
		seq = headSeq + index
	} else {
		seq = tailSeq + index + 1
	}
	if seq < headSeq || seq > tailSeq {
		return nil, nil
	}
	sk := lEncodeListKey(table, rk, seq)
	return db.eng.GetBytes(db.defaultReadOpts, sk)
}

func (db *RockDB) LVer(key []byte) (int64, error) {
	keyInfo, err := db.GetCollVersionKey(0, ListType, key, true)
	if err != nil {
		return 0, err
	}
	_, _, _, ts, err := parseListMeta(keyInfo.MetaData())
	return ts, err
}

func (db *RockDB) LLen(key []byte) (int64, error) {
	ts := time.Now().UnixNano()
	keyInfo, err := db.GetCollVersionKey(ts, ListType, key, true)
	if err != nil {
		return 0, err
	}
	if keyInfo.IsNotExistOrExpired() {
		return 0, nil
	}
	_, _, size, _, err := parseListMeta(keyInfo.MetaData())
	return int64(size), err
}

func (db *RockDB) LFixKey(ts int64, key []byte) {
	db.fixListKey(ts, key)
}

func (db *RockDB) LPop(ts int64, key []byte) ([]byte, error) {
	return db.lpop(ts, key, listHeadSeq)
}

func (db *RockDB) LTrim(ts int64, key []byte, start, stop int64) error {
	return db.ltrim2(ts, key, start, stop)
}

func (db *RockDB) LTrimFront(ts int64, key []byte, trimSize int64) (int64, error) {
	return db.ltrim(ts, key, trimSize, listHeadSeq)
}

func (db *RockDB) LTrimBack(ts int64, key []byte, trimSize int64) (int64, error) {
	return db.ltrim(ts, key, trimSize, listTailSeq)
}

func (db *RockDB) LPush(ts int64, key []byte, args ...[]byte) (int64, error) {
	if len(args) > MAX_BATCH_NUM {
		return 0, errTooMuchBatchSize
	}
	return db.lpush(ts, key, listHeadSeq, args...)
}
func (db *RockDB) LSet(ts int64, key []byte, index int64, value []byte) error {
	if err := checkKeySize(key); err != nil {
		return err
	}
	keyInfo, headSeq, tailSeq, size, _, err := db.lHeaderAndMeta(ts, key, false)
	if err != nil {
		return err
	}
	if keyInfo.IsNotExistOrExpired() {
		return errListIndex
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey
	if size == 0 {
		return errListIndex
	}

	var seq int64
	if index >= 0 {
		seq = headSeq + index
	} else {
		seq = tailSeq + index + 1
	}
	if seq < headSeq || seq > tailSeq {
		return errListIndex
	}
	wb := db.wb
	sk := lEncodeListKey(table, rk, seq)
	db.lSetMeta(key, keyInfo.OldHeader, headSeq, tailSeq, ts, wb)
	wb.Put(sk, value)
	err = db.CommitBatchWrite()
	return err
}

func (db *RockDB) LRange(key []byte, start int64, stop int64) ([][]byte, error) {
	if err := checkKeySize(key); err != nil {
		return nil, err
	}
	ts := time.Now().UnixNano()
	keyInfo, headSeq, tailSeq, llen, _, err := db.lHeaderAndMeta(ts, key, true)
	if err != nil {
		return nil, err
	}
	if keyInfo.IsNotExistOrExpired() {
		return [][]byte{}, nil
	}
	table := keyInfo.Table
	rk := keyInfo.VerKey

	if start < 0 {
		start = llen + start
	}
	if stop < 0 {
		stop = llen + stop
	}
	if start < 0 {
		start = 0
	}

	if start > stop || start >= llen {
		return [][]byte{}, nil
	}

	if stop >= llen {
		stop = llen - 1
	}

	limit := (stop - start) + 1
	if limit > MAX_BATCH_NUM {
		return nil, errTooMuchBatchSize
	}
	headSeq += start

	// TODO: use pool for large alloc
	v := make([][]byte, 0, limit)

	startKey := lEncodeListKey(table, rk, headSeq)
	stopKey := lEncodeListKey(table, rk, tailSeq)

	rit, err := engine.NewDBRangeLimitIterator(db.eng, startKey, stopKey, common.RangeClose, 0, int(limit), false)
	if err != nil {
		return nil, err
	}
	for ; rit.Valid(); rit.Next() {
		v = append(v, rit.Value())
	}
	rit.Close()
	return v, nil
}

func (db *RockDB) RPop(ts int64, key []byte) ([]byte, error) {
	return db.lpop(ts, key, listTailSeq)
}

func (db *RockDB) RPush(ts int64, key []byte, args ...[]byte) (int64, error) {
	if len(args) > MAX_BATCH_NUM {
		return 0, errTooMuchBatchSize
	}
	return db.lpush(ts, key, listTailSeq, args...)
}

func (db *RockDB) LClear(ts int64, key []byte) (int64, error) {
	if err := checkKeySize(key); err != nil {
		return 0, err
	}
	num := db.lDelete(ts, key, db.wb)
	//delete the expire data related to the list key
	db.delExpire(ListType, key, nil, false, db.wb)
	err := db.CommitBatchWrite()
	// num should be the deleted key number
	if num > 0 {
		return 1, err
	}
	return 0, err
}

func (db *RockDB) LMclear(keys ...[]byte) (int64, error) {
	if len(keys) > MAX_BATCH_NUM {
		return 0, errTooMuchBatchSize
	}

	for _, key := range keys {
		if err := checkKeySize(key); err != nil {
			return 0, err
		}
		db.lDelete(0, key, db.wb)
		db.delExpire(ListType, key, nil, false, db.wb)
	}
	err := db.CommitBatchWrite()
	if err != nil {
		// TODO: log here , the list maybe corrupt
	}

	return int64(len(keys)), err
}

func (db *RockDB) lMclearWithBatch(wb *gorocksdb.WriteBatch, keys ...[]byte) error {
	if len(keys) > MAX_BATCH_NUM {
		return errTooMuchBatchSize
	}

	for _, key := range keys {
		if err := checkKeySize(key); err != nil {
			return err
		}
		db.lDelete(0, key, wb)
		db.delExpire(ListType, key, nil, false, wb)
	}
	return nil
}

func (db *RockDB) LKeyExists(key []byte) (int64, error) {
	return db.collKeyExists(ListType, key)
}

func (db *RockDB) LExpire(ts int64, key []byte, duration int64) (int64, error) {
	return db.collExpire(ts, ListType, key, duration)
}

func (db *RockDB) LPersist(ts int64, key []byte) (int64, error) {
	return db.collPersist(ts, ListType, key)
}
