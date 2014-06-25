// +build appengine

package gcs

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gnd.la/blobstore/driver"
	"gnd.la/config"
	"gnd.la/internal"

	"appengine"
	"appengine/blobstore"
	"appengine/file"
)

var (
	errMissingBucketName = errors.New("missing bucket name and no default could be determined")
)

type gcsDriver struct {
	bucket string
	c      appengine.Context
}

type rfile struct {
	file.FileReader
}

func (f rfile) Metadata() ([]byte, error) {
	return nil, driver.ErrMetadataNotHandled
}

type wfile struct {
	io.WriteCloser
}

func (f wfile) SetMetadata(_ []byte) error {
	return driver.ErrMetadataNotHandled
}

func (d *gcsDriver) path(id string) string {
	return fmt.Sprintf("/gs/%s/%s", d.bucket, id)
}

func (d *gcsDriver) Create(id string) (driver.WFile, error) {
	f, _, err := file.Create(d.c, d.path(id), nil)
	if err != nil {
		return nil, err
	}
	return wfile{f}, nil
}

func (d *gcsDriver) Open(id string) (driver.RFile, error) {
	f, err := file.Open(d.c, d.path(id))
	if err != nil {
		return nil, err
	}
	return rfile{f}, nil
}

func (d *gcsDriver) Remove(id string) error {
	return file.Delete(d.c, d.path(id))
}

func (d *gcsDriver) Close() error {
	return nil
}

func (d *gcsDriver) Serve(w http.ResponseWriter, id string, rng driver.Range) (bool, error) {
	if rng.IsValid() {
		w.Header().Set("X-AppEngine-BlobRange", rng.String())
	}
	key, err := blobstore.BlobKeyForFile(d.c, d.path(id))
	if err != nil {
		return false, err
	}
	blobstore.Send(w, key)
	return true, nil
}

func (d *gcsDriver) SetContext(ctx appengine.Context) {
	d.c = ctx
}

func gcsOpener(url *config.URL) (driver.Driver, error) {
	value := url.Value
	if value == "" {
		if h := internal.AppEngineAppHost(); h != "" {
			value = strings.TrimPrefix(h, "http://")
		}
		if value == "" {
			return nil, errMissingBucketName
		}
	}
	return &gcsDriver{bucket: value}, nil
}

func init() {
	driver.Register("gcs", gcsOpener)
}
