package player

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/lbryio/lbry.go/v2/stream"
)

const defaultMaxCacheSize = 1 << 35 // 32GB

// ChunkCache can save and retrieve readable chunks.
type ChunkCache interface {
	Has(string) bool
	Get(string) (ReadableChunk, bool)
	Set(string, []byte) (ReadableChunk, error)
	Remove(string)
	Size() uint64
	WaitForRestore() error
}

type fsCache struct {
	storage  *fsStorage
	rCache   *ristretto.Cache
	resError chan error
}

// FSCacheOpts contains options for filesystem cache. Size is max size in bytes
type FSCacheOpts struct {
	Path          string
	Size          uint64
	SweepInterval time.Duration
}

type fsStorage struct {
	path string
}

type cachedChunk struct {
	reflectedChunk
}

// InitFSCache initializes disk cache for chunks.
// All chunk-sized files inside `dir` will be restored in the in-memory cache
// if `dir` does not exist, it will be created.
// In other words, os.TempDir() should not be passed as a `dir`.
func InitFSCache(opts *FSCacheOpts) (ChunkCache, error) {
	storage, err := initFSStorage(opts.Path)
	if err != nil {
		return nil, err
	}

	if opts.Size == 0 {
		opts.Size = defaultMaxCacheSize
	}

	if opts.SweepInterval == 0 {
		opts.SweepInterval = time.Second * 60
	}

	counters := opts.Size / ChunkSize * 10
	if counters <= 0 {
		counters = 10000
	}
	r, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: int64(counters),
		MaxCost:     int64(opts.Size),
		BufferItems: 64,
		Metrics:     true,
		OnEvict:     func(_, _ uint64, hash interface{}, _ int64) { storage.remove(hash) },
	})
	if err != nil {
		return nil, err
	}

	c := &fsCache{storage, r, make(chan error, 1)}

	sweepTicker := time.NewTicker(opts.SweepInterval)
	metricsTicker := time.NewTicker(500 * time.Millisecond)
	go func() {
		for {
			<-sweepTicker.C
			c.sweepChunks()
		}
	}()
	go func() {
		for {
			<-metricsTicker.C
			MtrCacheDroppedCount.Set(float64(r.Metrics.SetsDropped()))
			MtrCacheRejectedCount.Set(float64(r.Metrics.SetsRejected()))
			MtrCacheSize.Set(float64(c.Size()))
		}
	}()
	go func() {
		Logger.Infoln("restoring cache in memory...")
		err := c.reloadExistingChunks()
		if err != nil {
			Logger.Errorf("failed to restore cache in memory: %s", err.Error())
		} else {
			Logger.Infoln("done restoring cache in memory")
		}
		c.resError <- err
	}()

	return c, nil
}

func (c *fsCache) reloadExistingChunks() error {
	err := filepath.Walk(c.storage.path, func(path string, info os.FileInfo, err error) error {
		if c.storage.path == path {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			if len(info.Name()) != 1 {
				return fmt.Errorf("subfolder %v found inside cache folder", path)
			}
			return nil
		}
		if len(info.Name()) != stream.BlobHashHexLength {
			return fmt.Errorf("non-cache file found at path %v", path)
		}
		stored := c.set(info.Name(), info.Size())
		if !stored {
			Logger.Errorf("failed to restore blob %s in cache", info.Name())
		}
		return nil
	})
	return err
}

func initFSStorage(dir string) (*fsStorage, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}

	// Cache folder cleanup performed based on file names, chunk files will have a name of certain length.
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if dir == path {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			if len(info.Name()) != 1 {
				return fmt.Errorf("subfolder %v found inside cache folder", path)
			}
			return nil
		}
		if len(info.Name()) != stream.BlobHashHexLength {
			return fmt.Errorf("non-cache file found at path %v", path)
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return &fsStorage{dir}, nil
}

func (s fsStorage) remove(hash interface{}) {
	if err := os.Remove(s.getPath(hash)); err != nil {
		return
	}
}

func (s fsStorage) getPath(hash interface{}) string {
	return path.Join(s.path, hash.(string)[0:1], hash.(string))
}

func (s fsStorage) open(hash interface{}) (*os.File, error) {
	f, err := os.Open(s.getPath(hash))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Has returns true if cache contains the requested chunk.
// It is not guaranteed that actual file exists on the filesystem.
func (c *fsCache) Has(hash string) bool {
	_, ok := c.rCache.Get(hash)
	return ok
}

// WaitForRestore blocks execution until cache restore is complete and returns the resulting error (if any).
func (c *fsCache) WaitForRestore() error {
	err := <-c.resError
	close(c.resError)
	return err
}

// Get returns ReadableChunk if it can be retrieved from the cache by the requested hash
// and a boolean representing whether chunk was found or not.
func (c *fsCache) Get(hash string) (ReadableChunk, bool) {
	if value, ok := c.rCache.Get(hash); ok {
		f, err := c.storage.open(value)
		if err != nil {
			MtrCacheErrorCount.Inc()
			Logger.Errorf("chunk %v found in cache but couldn't open the file: %v", hash, err)
			c.rCache.Del(value)
			return nil, false
		}
		cb, err := initCachedChunk(f)
		if err != nil {
			Logger.Errorf("chunk %v found in cache but couldn't read the file: %v", hash, err)
			return nil, false
		}
		defer f.Close()
		return cb, true
	}

	Logger.Debugf("cache miss for chunk %v", hash)
	return nil, false
}

// Set takes chunk body and saves reference to it into the cache table
func (c *fsCache) Set(hash string, body []byte) (ReadableChunk, error) {
	cacheCost := len(body)

	Logger.Debugf("attempting to cache chunk %v", hash)
	chunkPath := c.storage.getPath(hash)
	err := os.MkdirAll(strings.Replace(chunkPath, hash, "", -1), 0700)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(chunkPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		MtrCacheErrorCount.Inc()
		Logger.Debugf("chunk %v already exists on the local filesystem, not overwriting", hash)
	} else {
		numWritten, err := f.Write(body)
		if err != nil {
			MtrCacheErrorCount.Inc()
			Logger.Errorf("error saving cache file %v: %v", chunkPath, err)
			return nil, err
		}

		err = f.Close()
		if err != nil {
			MtrCacheErrorCount.Inc()
			Logger.Errorf("error closing cache file %v: %v", chunkPath, err)
			return nil, err
		}

		Logger.Debugf("written %v bytes for chunk %v", numWritten, hash)
	}

	added := c.set(hash, int64(cacheCost))
	if !added {
		err := os.Remove(chunkPath)
		if err != nil {
			Logger.Errorf("chunk was not admitted and an error occurred removing chunk file: %v", err)
		} else {
			Logger.Infof("chunk %v was not admitted", hash)
		}
		return nil, err
	}
	Logger.Debugf("chunk %v successfully cached", hash)

	return &cachedChunk{reflectedChunk{body}}, nil
}

// set adds the entry in the cache. returns true if successful, false if unsuccessful
func (c *fsCache) set(hash string, cacheCost int64) bool {
	return c.rCache.Set(hash, hash, cacheCost)
}

// Remove deletes both cache record and chunk file from the filesystem.
func (c *fsCache) Remove(hash string) {
	c.storage.remove(hash)
	c.rCache.Del(hash)
}

func (c *fsCache) Size() uint64 {
	return c.rCache.Metrics.CostAdded() - c.rCache.Metrics.CostEvicted()
}

func (c *fsCache) sweepChunks() {
	var removed int
	err := filepath.Walk(c.storage.path, func(path string, info os.FileInfo, err error) error {
		if c.storage.path == path {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !c.Has(info.Name()) {
			err := os.Remove(path)
			if err == nil {
				removed++
			} else {
				return err
			}
		}
		return nil
	})
	if err != nil {
		Logger.Errorf("error sweeping cache folder: %v", err)
	} else {
		Logger.Infof("swept cache folder, %v chunks removed", removed)
	}
}

func initCachedChunk(file *os.File) (*cachedChunk, error) {
	body, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return &cachedChunk{reflectedChunk{body}}, nil
}
