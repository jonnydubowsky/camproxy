/*
Copyright 2013 Tamás Gulácsi

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package camutil

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/cacher"
	"camlistore.org/pkg/client"
	"camlistore.org/pkg/index"
	"camlistore.org/pkg/schema"
)

// Downloader is the struct for downloading file/dir blobs
type Downloader struct {
	cl   *client.Client
	dc   *cacher.DiskCache
	args []string
}

var (
	cachedClient    = make(map[string]*client.Client, 1)
	cachedClientMtx sync.Mutex
)

// NewClient returns a new client for the given server. Auth is set up according
// to the client config (~/.config/camlistore/client-config.json)
// and the environment variables.
func NewClient(server string) (*client.Client, error) {
	if server == "" {
		server = "localhost:3179"
	}
	cachedClientMtx.Lock()
	defer cachedClientMtx.Unlock()
	c, ok := cachedClient[server]
	if ok {
		return c, nil
	}
	c = client.New(server)
	if err := c.SetupAuth(); err != nil {
		return nil, err
	}
	cachedClient[server] = c
	return c, nil
}

var (
	cachedDownloader    = make(map[string]*Downloader, 1)
	cachedDownloaderMtx sync.Mutex
)

// The followings are copied from camlistore.org/cmd/camget

// NewDownloader creates a new Downloader (client + properties + disk cache)
// for the server
func NewDownloader(server string) (*Downloader, error) {
	cachedDownloaderMtx.Lock()
	defer cachedDownloaderMtx.Unlock()
	down, ok := cachedDownloader[server]
	if ok {
		return down, nil
	}

	down = new(Downloader)
	var err error
	if down.cl, err = NewClient(server); err != nil {
		return nil, err
	}

	down.cl.InsecureTLS = InsecureTLS
	tr := down.cl.TransportForConfig(&client.TransportConfig{
		Verbose: Verbose,
	})
	down.cl.SetHTTPClient(&http.Client{Transport: tr})

	down.dc, err = cacher.NewDiskCache(down.cl)
	if err != nil {
		return nil, fmt.Errorf("Error setting up local disk cache: %v", err)
	}
	if Verbose {
		log.Printf("Using temp blob cache directory %s", down.dc.Root)
	}
	if server != "" {
		down.args = []string{"-server=" + server}
	} else {
		down.args = []string{}
	}

	cachedDownloader[server] = down
	return down, nil
}

// Close closes the downloader (the underlying client)
func (down *Downloader) Close() {
	if down != nil && down.dc != nil {
		down.dc.Clean()
	}
}

// ParseBlobNames parses the blob names, appending to items, and returning
// the expanded slice, and error if happened.
// This uses blob.Parse, and can decode base64-encoded refs as a plus.
func ParseBlobNames(items []blob.Ref, names []string) ([]blob.Ref, error) {
	for _, arg := range names {
		br, ok := blob.Parse(arg)
		if !ok {
			var e error
			if br, e = Base64ToRef(arg); e != nil {
				return nil, e
			}
		}
		items = append(items, br)
	}
	return items, nil
}

// Base64ToRef decodes a base64-encoded blobref
func Base64ToRef(arg string) (br blob.Ref, err error) {
	b := make([]byte, 64)
	t := make([]byte, 2*len(b))
	var i, n int
	i = len(arg)
	if i > cap(t) {
		i = cap(t)
	}
	t = []byte(arg[:i])
	i = bytes.IndexByte(t, byte('-'))
	if i < 0 {
		err = fmt.Errorf("no - in %q", arg)
		return
	}
	n, err = base64.URLEncoding.Decode(b[:cap(b)], t[i+1:])
	if err != nil {
		err = fmt.Errorf("cannot decode %q as base64: %s", t[i+1:], err)
		return
	}
	b = b[:n]
	copy(t[:i], bytes.ToLower(t[:i]))
	t = t[:cap(t)]
	n = 2*len(b) - len(t) + n + 1
	if n > 0 {
		t = append(t, make([]byte, n)...)
	}
	//log.Printf("t=%q b=%q i=%d n=%d cap(t)=%d", t, b, i, n, cap(t))
	//log.Printf("b=[%d]%q", len(b), b)
	//log.Printf("t[i+1:]=[%d]%q", len(t), t[i+1:])
	n = hex.Encode(t[i+1:], b)
	arg = string(t[:i+1+n])
	br, ok := blob.Parse(arg)
	if !ok {
		err = fmt.Errorf("cannot parse %q as blobref", arg)
		return
	}
	return br, nil
}

// Start starts the downloads of the blobrefs.
// Just the JSON schema if contents is false, else the content of the blob.
func (down *Downloader) Start(contents bool, items ...blob.Ref) (io.ReadCloser, error) {
	readers := make([]io.Reader, 0, len(items))
	closers := make([]io.Closer, 0, len(items))
	var (
		rc  io.ReadCloser
		err error
	)
	for _, br := range items {
		if contents {
			rc, err = schema.NewFileReader(down.dc, br)
			if err == nil {
				rc.(*schema.FileReader).LoadAllChunks()
			}
		} else {
			rc, err = fetch(down.dc, br)
		}
		if err == nil {
			readers = append(readers, rc)
			closers = append(closers, rc)
			continue
		}
		log.Printf("error downloading %q: %s", br, err)
		args := append(make([]string, 0, len(down.args)+3), down.args...)
		if contents {
			args = append(args, "-contents=true")
		}
		if InsecureTLS {
			args = append(args, "-insecure=true")
		}
		args = append(args, br.String())
		c := exec.Command("camget", args...)
		errbuf := bytes.NewBuffer(nil)
		c.Stderr = errbuf
		if rc, err = c.StdoutPipe(); err != nil {
			return nil, fmt.Errorf("error createing stdout pipe for camget %q: %s (%v)", args, errbuf.String(), err)
		}
		log.Printf("calling camget %q", args)
		if err = c.Run(); err != nil {
			return nil, fmt.Errorf("error calling camget %q: %s (%v)", args, errbuf.String(), err)
		}
		readers = append(readers, rc)
		closers = append(closers, rc)
	}

	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(readers...),
		multiCloser{closers},
	}, nil
}

// Save saves contents of the blobs into destDir as files
func (down *Downloader) Save(destDir string, contents bool, items ...blob.Ref) error {
	for _, br := range items {
		if err := smartFetch(down.dc, destDir, br); err != nil {
			log.Fatal(err)
		}
	}
	return nil
}

func fetch(src blob.Fetcher, br blob.Ref) (r io.ReadCloser, err error) {
	if Verbose {
		log.Printf("Fetching %s", br.String())
	}
	r, _, err = src.Fetch(br)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch %s: %s", br, err)
	}
	return r, err
}

// A little less than the sniffer will take, so we don't truncate.
const sniffSize = 900 * 1024

// smartFetch the things that blobs point to, not just blobs.
func smartFetch(src blob.Fetcher, targ string, br blob.Ref) error {
	rc, err := fetch(src, br)
	if err != nil {
		return err
	}
	defer rc.Close()

	sniffer := index.NewBlobSniffer(br)
	_, err = io.CopyN(sniffer, rc, sniffSize)
	if err != nil && err != io.EOF {
		return err
	}

	sniffer.Parse()
	b, ok := sniffer.SchemaBlob()

	if !ok {
		if Verbose {
			log.Printf("Fetching opaque data %v into %q", br, targ)
		}

		// opaque data - put it in a file
		f, err := os.Create(targ)
		if err != nil {
			return fmt.Errorf("opaque: %v", err)
		}
		defer f.Close()
		body, _ := sniffer.Body()
		r := io.MultiReader(bytes.NewReader(body), rc)
		_, err = io.Copy(f, r)
		return err
	}

	switch b.Type() {
	case "directory":
		dir := filepath.Join(targ, b.FileName())
		if Verbose {
			log.Printf("Fetching directory %v into %s", br, dir)
		}
		if err := os.MkdirAll(dir, b.FileMode()); err != nil {
			return err
		}
		if err := setFileMeta(dir, b); err != nil {
			log.Print(err)
		}
		entries, ok := b.DirectoryEntries()
		if !ok {
			return fmt.Errorf("bad entries blobref in dir %v", b.BlobRef())
		}
		return smartFetch(src, dir, entries)
	case "static-set":
		if Verbose {
			log.Printf("Fetching directory entries %v into %s", br, targ)
		}

		// directory entries
		const numWorkers = 10
		type work struct {
			br   blob.Ref
			errc chan<- error
		}
		members := b.StaticSetMembers()
		workc := make(chan work, len(members))
		defer close(workc)
		for i := 0; i < numWorkers; i++ {
			go func() {
				for wi := range workc {
					wi.errc <- smartFetch(src, targ, wi.br)
				}
			}()
		}
		var errcs []<-chan error
		for _, mref := range members {
			errc := make(chan error, 1)
			errcs = append(errcs, errc)
			workc <- work{mref, errc}
		}
		for _, errc := range errcs {
			if err := <-errc; err != nil {
				return err
			}
		}
		return nil
	case "file":
		fr, err := schema.NewFileReader(src, br)
		if err != nil {
			return fmt.Errorf("NewFileReader: %v", err)
		}
		fr.LoadAllChunks()
		defer fr.Close()

		name := filepath.Join(targ, b.FileName())

		if fi, err := os.Stat(name); err == nil && fi.Size() == fi.Size() {
			if Verbose {
				log.Printf("Skipping %s; already exists.", name)
				return nil
			}
		}

		if Verbose {
			log.Printf("Writing %s to %s ...", br, name)
		}

		f, err := os.Create(name)
		if err != nil {
			return fmt.Errorf("file type: %v", err)
		}
		defer f.Close()
		if _, err := io.Copy(f, fr); err != nil {
			return fmt.Errorf("Copying %s to %s: %v", br, name, err)
		}
		if err := setFileMeta(name, b); err != nil {
			log.Print(err)
		}
		return nil
	default:
		return errors.New("unknown blob type: " + b.Type())
	}
}

func setFileMeta(name string, blob *schema.Blob) error {
	err1 := os.Chmod(name, blob.FileMode())
	var err2 error
	if mt := blob.ModTime(); !mt.IsZero() {
		err2 = os.Chtimes(name, mt, mt)
	}
	// TODO: we previously did os.Chown here, but it's rarely wanted,
	// then the schema.Blob refactor broke it, so it's gone.
	// Add it back later once we care?
	for _, err := range []error{err1, err2} {
		if err != nil {
			return err
		}
	}
	return nil
}

var _ = io.Closer(multiCloser{})

type multiCloser struct {
	closers []io.Closer
}

func (mc multiCloser) Close() error {
	var err error
	for _, c := range mc.closers {
		if closeErr := c.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}
