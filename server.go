package conf

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/minio/blake2b-simd"
)

// Server is used to serve http requests and acts as a config.
type Server struct {
	DB         *gorm.DB
	Encoding   *base32.Encoding
	Expiration time.Duration
	Max        int64
	FilePath   string
	TempPath   string
	PublicPath string
}

// Cleanup will delete expired content and remove files associated with it as
// long as it is not used by any other content.
func (s Server) Cleanup() error {
	if err := s.DB.Delete(&Content{}, "expires < ?", time.Now()).Error; err != nil {
		return err
	}
	return filepath.Walk(s.FilePath, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if f.IsDir() || filepath.Dir(path) != s.FilePath {
			return nil
		}
		if !s.DB.Where("checksum = ?", f.Name()).Find(&Content{}).RecordNotFound() {
			return nil
		}
		return os.Remove(filepath.Join(s.FilePath, f.Name()))
	})
}

// ServeHTTP will serve HTTP requests. /, /css, /fonts, /js and /upload are all
// static routes and any other route is considered a request for content.
func (s Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO: version?
	w.Header().Set("Server", "conf")
	hasPrefix := func(s string) bool {
		return strings.HasPrefix(r.URL.Path, s)
	}
	switch {
	case r.URL.Path == "/upload":
		s.UploadHandler(w, r)
	case r.URL.Path == "/", hasPrefix("/css"), hasPrefix("/fonts"), hasPrefix("/js"):
		s.StaticHandler(w, r)
	default:
		s.ContentHandler(w, r)
	}
}

// StaticHandler will server static content given the url path.
func (s Server) StaticHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		return
	case r.Method != http.MethodHead && r.Method != http.MethodGet:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	http.ServeFile(w, r, path.Join(s.PublicPath, r.URL.Path))
}

// ContentHandler will query the database for the given slug. If the slug doesn't exist it
// will return 404 otherwise it will serve the file.
func (s Server) ContentHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		return
	case r.Method != http.MethodHead && r.Method != http.MethodGet:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	// decode split the path to allow for extensions
	b, err := s.Encoding.DecodeString(strings.Split(r.URL.Path, ".")[0][1:])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var c Content
	if s.DB.First(&c, "id = ?", b).RecordNotFound() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.FilePath, c.Checksum))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Detect content type before serving content to filter html files
	ctype := mime.TypeByExtension(filepath.Ext(c.Name))
	if ctype == "" {
		var b [512]byte
		n, _ := io.ReadFull(f, b[:])
		ctype = http.DetectContentType(b[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "seeker can't seek", http.StatusInternalServerError)
			return
		}
	}
	// catches text/html and text/html; charset=utf-8
	if strings.HasPrefix(ctype, "text/html") {
		ctype = "text/plain; charset=utf-8"
	}
	// 1 year
	cache := "31536000"
	if e := c.Expires; e != nil {
		// duration in seconds until expiration
		d := int(time.Until(*e).Seconds())
		// if expired then the request should return 404 and send the content to
		// the cleanup worker -- for now we won't do anything and we'll let the
		// worker clean up content in its own time.
		if d > 0 {
			cache = fmt.Sprintf("private, must-revalidate, max-age=%d", d)
		} else {
			cache = "no-cache"
		}
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Disposition", fmt.Sprintf("filename=%q", c.Name))
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Etag", strconv.Quote(c.Checksum))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, c.Name, c.CreatedAt, f)
}

// UploadHandler serves as a handler for uploading files to conf. It will
// generate a random key and iv then both hash and encrypt the body. After that,
// conf generates a secret (sum[32:48] ^ key) as well as a MAC using the
// secret as the key and sum[32:48] as the body. The expiration date and path is
// then sent to the client in JSON form.
func (s Server) UploadHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "OPTIONS, POST")
		return
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", "OPTIONS, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	case r.ContentLength > s.Max:
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	// Find the multipart body to read from.
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var p *multipart.Part
	for {
		p, err = mr.NextPart()
		if err == io.EOF {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if p.FormName() == "file" {
			break
		}
	}
	defer p.Close()
	name := p.FileName()
	if name == "" || len(name) > 255 {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	// Create temporary file to be used for storing uploads.
	tf, err := ioutil.TempFile(s.TempPath, "conf-upload")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// If the file is uploaded successfully and renamed this operation will fail.
	defer os.Remove(tf.Name())
	defer tf.Close()
	// Hash and save the file.
	h := blake2b.New512()
	n, err := io.Copy(io.MultiWriter(tf, h), http.MaxBytesReader(w, p, s.Max))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// base64 for sum encoding since it's slightly more compact than base32 and
	// is unlikely to be read by humans.
	sum := base64.RawURLEncoding.EncodeToString(h.Sum(nil))
	// Find the content
	if s.DB.First(&Content{}, "checksum = ?", sum).RecordNotFound() {
		p := filepath.Join(s.FilePath, sum)
		if err := os.Rename(tf.Name(), p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	c := Content{
		Checksum: sum,
		Name:     name,
		Size:     n,
	}
	if s.Expiration > 0 {
		e := time.Now().Add(s.Expiration)
		c.Expires = &e
	}
	if err := s.DB.Create(&c).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(struct {
		Expires *time.Time `json:"expires,omitempty"`
		Path    string     `json:"path"`
	}{c.Expires, s.Encoding.EncodeToString(c.ID) + filepath.Ext(c.Name)})
}
