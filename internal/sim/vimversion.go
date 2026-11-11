package sim

import (
	"bytes"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// since90 matches the datastore members that vim 9.0.0.0 added. A vCenter
// leaves out members newer than the API version a client asks for, and a
// client built for vim 8.x (PowerCLI 13.3 asks for 8.0.3.0) cannot decode a
// VmfsDatastoreInfo that carries them: it drops the vmfs volume.
var since90 = regexp.MustCompile(`<supportedVDiskFormats>[^<]*</supportedVDiskFormats>|<logicalSectorSize>[^<]*</logicalSectorSize>|<physicalSectorSize>[^<]*</physicalSectorSize>`)

// predates90 reports whether the request's SOAPAction names a vim version
// before 9.0, as in "urn:vim25/8.0.3.0". Anything else is served in full.
func predates90(r *http.Request) bool {
	action := strings.Trim(r.Header.Get("SOAPAction"), `"`)
	version, ok := strings.CutPrefix(action, "urn:vim25/")
	if !ok {
		return false
	}
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n < 9
}

// bufferedResponse holds a response so its members can be filtered before
// it is sent.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	return b.body.Write(p)
}

// serveBefore90 answers a pre-9.0 client with the members it cannot decode
// removed.
func serveBefore90(w http.ResponseWriter, r *http.Request, next http.Handler) {
	r.Header.Del("Accept-Encoding")
	b := &bufferedResponse{header: w.Header()}
	next.ServeHTTP(b, r)
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(b.status)
	_, _ = w.Write(since90.ReplaceAll(b.body.Bytes(), nil))
}
