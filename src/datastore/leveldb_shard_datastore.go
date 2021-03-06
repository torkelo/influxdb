package datastore

import (
	"bytes"
	"cluster"
	log "code.google.com/p/log4go"
	"configuration"
	"fmt"
	"github.com/jmhodges/levigo"
	"os"
	"path/filepath"
	"protocol"
	"sync"
)

type LevelDbShardDatastore struct {
	baseDbDir      string
	config         *configuration.Configuration
	shards         map[uint32]*LevelDbShard
	shardsLock     sync.RWMutex
	levelDbOptions *levigo.Options
	writeBuffer    *cluster.WriteBuffer
}

const (
	ONE_KILOBYTE                    = 1024
	ONE_MEGABYTE                    = 1024 * 1024
	ONE_GIGABYTE                    = ONE_MEGABYTE * 1024
	TWO_FIFTY_SIX_KILOBYTES         = 256 * 1024
	MAX_SERIES_SIZE                 = ONE_MEGABYTE
	DATABASE_DIR                    = "db"
	SHARD_BLOOM_FILTER_BITS_PER_KEY = 10
	SHARD_DATABASE_DIR              = "shard_db"
)

var (

	// This datastore implements the PersistentAtomicInteger interface. All of the persistent
	// integers start with this prefix, followed by their name
	ATOMIC_INCREMENT_PREFIX = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFD}
	// NEXT_ID_KEY holds the next id. ids are used to "intern" timeseries and column names
	NEXT_ID_KEY = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	// SERIES_COLUMN_INDEX_PREFIX is the prefix of the series to column names index
	SERIES_COLUMN_INDEX_PREFIX = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFE}
	// DATABASE_SERIES_INDEX_PREFIX is the prefix of the database to series names index
	DATABASE_SERIES_INDEX_PREFIX = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	MAX_SEQUENCE                 = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	// replicateWrite = protocol.Request_REPLICATION_WRITE

	TRUE = true
)

type Field struct {
	Id   []byte
	Name string
}

type rawColumnValue struct {
	time     []byte
	sequence []byte
	value    []byte
}

func NewLevelDbShardDatastore(config *configuration.Configuration) (*LevelDbShardDatastore, error) {
	baseDbDir := filepath.Join(config.DataDir, SHARD_DATABASE_DIR)
	err := os.MkdirAll(baseDbDir, 0744)
	if err != nil {
		return nil, err
	}
	opts := levigo.NewOptions()
	opts.SetCache(levigo.NewLRUCache(ONE_MEGABYTE))
	opts.SetCreateIfMissing(true)
	opts.SetBlockSize(64 * ONE_KILOBYTE)
	filter := levigo.NewBloomFilter(SHARD_BLOOM_FILTER_BITS_PER_KEY)
	opts.SetFilterPolicy(filter)
	opts.SetMaxOpenFiles(config.LevelDbMaxOpenFiles)

	return &LevelDbShardDatastore{
		baseDbDir:      baseDbDir,
		config:         config,
		shards:         make(map[uint32]*LevelDbShard),
		levelDbOptions: opts,
	}, nil
}

func (self *LevelDbShardDatastore) Close() {
	self.shardsLock.Lock()
	defer self.shardsLock.Unlock()
	for _, shard := range self.shards {
		shard.close()
	}
}

func (self *LevelDbShardDatastore) GetOrCreateShard(id uint32) (cluster.LocalShardDb, error) {
	self.shardsLock.RLock()
	db := self.shards[id]
	self.shardsLock.RUnlock()

	if db != nil {
		return db, nil
	}

	self.shardsLock.Lock()
	defer self.shardsLock.Unlock()

	// check to make sure it hasn't been put there between the RUnlock and the Lock
	db = self.shards[id]
	if db != nil {
		return db, nil
	}

	dbDir := self.shardDir(id)

	log.Info("DATASTORE: opening or creating shard %s", dbDir)
	ldb, err := levigo.Open(dbDir, self.levelDbOptions)
	if err != nil {
		return nil, err
	}

	db, err = NewLevelDbShard(ldb)
	if err != nil {
		return nil, err
	}
	self.shards[id] = db
	return db, nil
}

func (self *LevelDbShardDatastore) Write(request *protocol.Request) error {
	shardDb, err := self.GetOrCreateShard(*request.ShardId)
	if err != nil {
		return err
	}
	return shardDb.Write(*request.Database, request.Series)
}

func (self *LevelDbShardDatastore) BufferWrite(request *protocol.Request) {
	self.writeBuffer.Write(request)
}

func (self *LevelDbShardDatastore) SetWriteBuffer(writeBuffer *cluster.WriteBuffer) {
	self.writeBuffer = writeBuffer
}

func (self *LevelDbShardDatastore) DeleteShard(shardId uint32) error {
	self.shardsLock.Lock()
	shardDb := self.shards[shardId]
	delete(self.shards, shardId)
	self.shardsLock.Unlock()

	if shardDb != nil {
		shardDb.close()
	}

	dir := self.shardDir(shardId)
	log.Info("DATASTORE: dropping shard %s", dir)
	return os.RemoveAll(dir)
}

func (self *LevelDbShardDatastore) shardDir(id uint32) string {
	return filepath.Join(self.baseDbDir, fmt.Sprintf("%.5d", id))
}

// // returns true if the point has the correct field id and is
// // in the given time range
func isPointInRange(fieldId, startTime, endTime, point []byte) bool {
	id := point[:8]
	time := point[8:16]
	return bytes.Equal(id, fieldId) && bytes.Compare(time, startTime) > -1 && bytes.Compare(time, endTime) < 1
}

type FieldLookupError struct {
	message string
}

func (self FieldLookupError) Error() string {
	return self.message
}

// depending on the query order (whether it's ascending or not) returns
// the min (or max in case of descending query) of the current
// [timestamp,sequence] and the self's [timestamp,sequence]
//
// This is used to determine what the next point's timestamp
// and sequence number should be.
func (self *rawColumnValue) updatePointTimeAndSequence(currentTimeRaw, currentSequenceRaw []byte, isAscendingQuery bool) ([]byte, []byte) {
	if currentTimeRaw == nil {
		return self.time, self.sequence
	}

	compareValue := 1
	if isAscendingQuery {
		compareValue = -1
	}

	timeCompare := bytes.Compare(self.time, currentTimeRaw)
	if timeCompare == compareValue {
		return self.time, self.sequence
	}

	if timeCompare != 0 {
		return currentTimeRaw, currentSequenceRaw
	}

	if bytes.Compare(self.sequence, currentSequenceRaw) == compareValue {
		return currentTimeRaw, self.sequence
	}

	return currentTimeRaw, currentSequenceRaw
}
