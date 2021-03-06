// Copyright (c) 2021 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package minfs contains the MinFS core package
package minfs

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/minio/minfs/meta"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

var (
	_ = meta.RegisterExt(1, File{})
	_ = meta.RegisterExt(2, Dir{})
)

// Keyed Mutex
type KeyedMutex struct {
	mutexes sync.Map // Zero value is empty and ready for use
}

// This lets us lock resources via a key (we'll use it to lock overlapping Open requests to prevent data-race condition between cacheAllocate and cacheSave)
func (m *KeyedMutex) Lock(key string) func() {
	value, _ := m.mutexes.LoadOrStore(key, &sync.Mutex{})
	mtx := value.(*sync.Mutex)
	mtx.Lock()

	return func() { mtx.Unlock() }
}

// MinFS contains the meta data for the MinFS client
type MinFS struct {
	config *Config
	api    *minio.Client

	db *meta.DB

	// Logger instance.
	log *log.Logger

	// contains all open handles
	handles []*FileHandle

	// Tracks fuse open requests
	fdcounter uint64

	locks   map[string]bool
	openfds map[uint64]string

	// Global openfd map lock
	m sync.Mutex

	syncChan chan interface{}

	listenerDoneCh chan struct{}

	// Keyed cache resource lock
	km KeyedMutex
}

// New will return a new MinFS client
func New(options ...func(*Config)) (*MinFS, error) {
	// Initialize config.
	ac, err := InitMinFSConfig()
	if err != nil {
		return nil, err
	}

	// Initialize log file.
	logW, err := os.OpenFile(globalLogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		return nil, err
	}

	// Set defaults
	cfg := &Config{
		cache:     globalDBDir,
		quota:     globalQuota,
		basePath:  "",
		accountID: fmt.Sprintf("%d", time.Now().UTC().Unix()),
		gid:       0,
		uid:       0,
		accessKey: ac.AccessKey,
		secretKey: ac.SecretKey,
		mode:      os.FileMode(0444),
	}

	for _, optionFn := range options {
		optionFn(cfg)
	}

	// Create db directory.
	if err := os.MkdirAll(cfg.cache, 0777); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// Initialize MinFS.
	fs := &MinFS{
		config:         cfg,
		syncChan:       make(chan interface{}),
		locks:          map[string]bool{},
		openfds:        map[uint64]string{},
		log:            log.New(logW, "MinFS ", log.Ldate|log.Ltime|log.Lshortfile),
		listenerDoneCh: make(chan struct{}),
	}

	// Success..
	return fs, nil
}

func (mfs *MinFS) mount() (*fuse.Conn, error) {
	mfs.log.Println("Mounting target...", mfs.config.mountpoint)
	return fuse.Mount(
		mfs.config.mountpoint,
		fuse.FSName("mskvfs"),
		fuse.Subtype("mskvfs"),
		fuse.AllowOther(),
	)
}

func (mfs *MinFS) getApi(uid uint32) (api *minio.Client, err error) {

	var (
		host   = mfs.config.target.Host
		access = mfs.config.accessKey
		secret = mfs.config.secretKey
		token  = mfs.config.secretToken
		secure = mfs.config.target.Scheme == "https"
	)

	var transport http.RoundTripper = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
		// Set this value so that the underlying transport round-tripper
		// doesn't try to auto decode the body of objects with
		// content-encoding set to `gzip`.
		//
		// Refer:
		//    https://golang.org/src/net/http/transport.go?h=roundTrip#L1843
		DisableCompression: true,
	}

	creds := credentials.NewStaticV4(access, secret, token)
	options := &minio.Options{
		Creds:     creds,
		Secure:    secure,
		Transport: transport,
	}

	api, err = minio.New(host, options)

	return api, err
}

// Serve starts the MinFS client
func (mfs *MinFS) Serve() (err error) {
	if mfs.config.debug {
		fuse.Debug = func(msg interface{}) {
			mfs.log.Printf("%#v\n", msg)
		}
	}

	defer mfs.shutdown()

	// mount the drive
	var c *fuse.Conn
	c, err = mfs.mount()
	if err != nil {
		return err
	}

	defer c.Close()

	// channel to receive errors
	trapChannel := signalTrap(os.Interrupt, syscall.SIGTERM, os.Kill)

	go func() {
		<-trapChannel

		mfs.log.Println("Intercepted trapChannel signal, attempting graceful shutdown")

		mfs.shutdown()

	}()

	// Initialize database.
	mfs.log.Println("Opening cache database")
	mfs.db, err = meta.Open(path.Join(mfs.config.cache, "meta", "cache.db"), 0600, nil)
	if err != nil {
		return err
	}
	defer mfs.db.Close()

	mfs.log.Println("Initializing cache database")
	if err = mfs.db.Update(func(tx *meta.Tx) error {
		_, berr := tx.CreateBucketIfNotExists([]byte("minio/"))
		return berr
	}); err != nil {
		return err
	}

	mfs.log.Println("Initializing minio client:")
	var (
		host   = mfs.config.target.Host
		access = mfs.config.accessKey
		secret = mfs.config.secretKey
		token  = mfs.config.secretToken
		secure = mfs.config.target.Scheme == "https"
	)

	go mfs.MonitorCache()

	var transport http.RoundTripper = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: mfs.config.insecure,
		},
		// Set this value so that the underlying transport round-tripper
		// doesn't try to auto decode the body of objects with
		// content-encoding set to `gzip`.
		//
		// Refer:
		//    https://golang.org/src/net/http/transport.go?h=roundTrip#L1843
		DisableCompression: true,
	}

	creds := credentials.NewStaticV4(access, secret, token)
	options := &minio.Options{
		Creds:     creds,
		Secure:    secure,
		Transport: transport,
	}

	mfs.api, err = minio.New(host, options)
	if err != nil {
		return err
	}

	if err = mfs.startSync(); err != nil {
		return err
	}

	mfs.log.Println("Serving... Have fun!")
	// Serve the filesystem
	if err = fs.Serve(c, mfs); err != nil {
		mfs.log.Println("Error while serving the file system.", err)
		return err
	}

	<-c.Ready

	fmt.Println("\nMount process complete, graceful shutdown")
	return c.MountError
}

func (mfs *MinFS) shutdown() {
	mfs.log.Println("Shutting down")

	if err := fuse.Unmount(mfs.config.mountpoint); err != nil {
		mfs.log.Println("Some error (possibly ok) while umounting", mfs.config.mountpoint, err)
	}

}

func (mfs *MinFS) sync(req interface{}) error {
	mfs.syncChan <- req
	return nil
}

func (mfs *MinFS) moveOp(req *MoveOperation) {
	fmt.Println("moveOp() removed")
}

func (mfs *MinFS) copyOp(req *CopyOperation) {
	fmt.Println("copyOp() removed")
}

func (mfs *MinFS) putOp(req *PutOperation) {
	fmt.Println("putOp() removed")
}

func (mfs *MinFS) startSync() error {
	go func() {
		for req := range mfs.syncChan {
			switch req := req.(type) {
			case *MoveOperation:
				mfs.moveOp(req)
			case *CopyOperation:
				mfs.copyOp(req)
			case *PutOperation:
				mfs.putOp(req)
			default:
				panic("Unknown type")
			}
		}
	}()
	return nil
}

// Statfs will return meta information on the minio filesystem
func (mfs *MinFS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {
	resp.Blocks = 0x1000000000
	resp.Bfree = 0x1000000000
	resp.Bavail = 0x1000000000
	resp.Namelen = 32768
	resp.Bsize = 1024
	return nil
}

// Acquire will return a new FileHandle, adds to openfd map
func (mfs *MinFS) Acquire(f *File, resourceKey string) (*FileHandle, error) {

	fh := &FileHandle{
		f: f,
	}

	// Every new open request gets it's own ID
	atomic.AddUint64(&mfs.fdcounter, 1)
	fh.handle = mfs.fdcounter

	mfs.m.Lock()
	mfs.openfds[fh.handle] = resourceKey
	mfs.m.Unlock()

	return fh, nil
}

// Release release the filehandle, removes from openfd map
func (mfs *MinFS) Release(fh *FileHandle) error {

	mfs.m.Lock()
	delete(mfs.openfds, fh.handle)
	mfs.m.Unlock()

	return nil
}

// NextSequence will return the next free iNode
func (mfs *MinFS) NextSequence(tx *meta.Tx) (sequence uint64, err error) {
	bucket := tx.Bucket("minio/")
	return bucket.NextSequence()
}

// Root is the root folder of the MinFS mountpoint
func (mfs *MinFS) Root() (fs.Node, error) {
	return &Dir{
		dir:  nil,
		mfs:  mfs,
		Path: "",

		UID:  mfs.config.uid,
		GID:  mfs.config.gid,
		Mode: os.ModeDir | 0750,
	}, nil
}

// Storer -
type Storer interface {
	store(tx *meta.Tx)
}

// NewCachePath -
func (mfs *MinFS) NewCachePath() (string, error) {
	cachePath := path.Join(mfs.config.cache, nextSuffix())
	for {
		if _, err := os.Stat(cachePath); err == nil {
		} else if os.IsNotExist(err) {
			return cachePath, nil
		} else {
			return "", err
		}
		cachePath = path.Join(mfs.config.cache, nextSuffix())
	}
}
