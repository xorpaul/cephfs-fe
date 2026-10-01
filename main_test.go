package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{c, key}
}

// issue returns a leaf certificate as a tls.Certificate plus its PEM files.
func (ca *testCA) issue(t *testing.T, cn string, server bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	tc, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return tc, certPEM, keyPEM
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClientCertAuth(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "test CA")
	_, srvCert, srvKey := ca.issue(t, "server", true)
	write(t, filepath.Join(dir, "srv.pem"), srvCert)
	write(t, filepath.Join(dir, "srv.key"), srvKey)
	write(t, filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}))
	cns := filepath.Join(dir, "cns")
	// Entries are normalized: attribute case and spaces don't matter.
	write(t, cns, []byte("# admins\nCN=alice\n  cn = carol  \n"))

	cm, err := newCertManager(filepath.Join(dir, "srv.pem"), filepath.Join(dir, "srv.key"), filepath.Join(dir, "ca.pem"), cns, "")
	if err != nil {
		t.Fatal(err)
	}
	// Serve the way main does: ServeTLS with no file names, so the
	// certificate comes only from the certManager.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: cm.requireClientCert(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "hello "+clientID(r)+" "+r.Proto)
		})),
		TLSConfig: cm.tlsConfig(),
		ErrorLog:  log.New(io.Discard, "", 0), // the foreign-CA handshake failure is expected
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ServeTLS(ln, "", "") }()
	defer srv.Close()
	baseURL := "https://" + ln.Addr().String()

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	get := func(cert *tls.Certificate) (int, string, error) {
		cfg := &tls.Config{RootCAs: roots}
		if cert != nil {
			// Always send it: the default client only offers a certificate
			// whose issuer the server listed as acceptable.
			cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}}
		r, err := c.Get(baseURL)
		if err != nil {
			select {
			case serr := <-serveErr:
				t.Fatalf("server stopped: %v", serr)
			default:
			}
			return 0, "", err
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b), nil
	}

	alice, _, _ := ca.issue(t, "alice", false)
	bob, _, _ := ca.issue(t, "bob", false)
	carol, _, _ := ca.issue(t, "carol", false)
	mallory, _, _ := newCA(t, "other CA").issue(t, "alice", false)

	if code, body, err := get(nil); err != nil || code != http.StatusUnauthorized || !strings.Contains(body, "Client Certificate Required") {
		t.Errorf("no cert: %d %v", code, err)
	}
	if code, body, err := get(&bob); err != nil || code != http.StatusForbidden || !strings.Contains(body, "CN=bob") {
		t.Errorf("bob: %d %v", code, err)
	}
	for _, c := range []*tls.Certificate{&alice, &carol} {
		if code, body, err := get(c); err != nil || code != http.StatusOK || !strings.HasPrefix(body, "hello ") || !strings.HasSuffix(body, "HTTP/2.0") {
			t.Errorf("allowed: %d %q %v", code, body, err)
		}
	}
	if code, _, err := get(&mallory); err == nil {
		t.Errorf("certificate from another CA: handshake succeeded, status %d", code)
	}

	// Allow bob: the change is picked up by mtime and applies at once.
	write(t, cns, []byte("CN=alice\nCN=bob\n"))
	os.Chtimes(cns, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if !cm.changed() {
		t.Fatal("subjects file change not detected")
	}
	if err := cm.reload(); err != nil {
		t.Fatal(err)
	}
	if code, body, err := get(&bob); err != nil || code != http.StatusOK || body != "hello CN=bob HTTP/2.0" {
		t.Errorf("bob after reload: %d %q %v", code, body, err)
	}
	if code, _, _ := get(&carol); code != http.StatusForbidden {
		t.Errorf("carol after reload: %d", code)
	}

	// A broken file keeps the previous state.
	write(t, filepath.Join(dir, "ca.pem"), []byte("garbage"))
	if err := cm.reload(); err == nil {
		t.Error("reload with a broken CA succeeded")
	}
	if code, _, _ := get(&bob); code != http.StatusOK {
		t.Errorf("bob after failed reload: %d", code)
	}
}

func TestParseSearch(t *testing.T) {
	for _, tc := range []struct {
		query string
		ok    bool
		re    string
	}{
		{"pattern=index.php", true, `^index\.php$`}, // exact is the default
		{"pattern=index.php&match=exact", true, `^index\.php$`},
		{"pattern=index.php&match=prefix", true, `^index\.php`},
		{"pattern=index.php&match=contains", true, `index\.php`},
		{"pattern=index.php&match=regex", true, `index.php`},
		{"pattern=index.php&regex=1", true, `index.php`}, // legacy regex=1
		{"pattern=%5Etmp_&match=regex", true, `^tmp_`},
		{"pattern=a%2Bb&match=contains&type=file&uid=33&newer=2026-09-01&fs=vol1,vol2", true, `a\+b`},
		{"pattern=x", true, `^x$`}, // exact and prefix use the index, so one character is fine
		{"pattern=x&match=prefix", true, `^x`},
		{"pattern=x&match=contains", false, ""},
		{"pattern=ab&match=fuzzy", false, ""},
		{"pattern=", false, ""},
		{"pattern=.*&match=regex", false, ""},
		{"pattern=(?i)app-config&match=regex", false, ""},
		{"pattern=foo|bar&match=regex", false, ""},
		{"pattern=(&match=regex", false, ""},
		{"pattern=ab&type=pipe", false, ""},
		{"pattern=ab&uid=-1", false, ""},
		{"pattern=ab&newer=yesterday", false, ""},
		{"pattern=ab&fs=vol1%3Bdrop", false, ""},
		{"pattern=ab&fs=VOL1", false, ""},
	} {
		v, _ := url.ParseQuery(tc.query)
		sr, err := parseSearch(v, 1000)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.query, err, tc.ok)
			continue
		}
		if err != nil {
			if code := err.(*apiError).status; code != http.StatusBadRequest {
				t.Errorf("%s: status %d", tc.query, code)
			}
			continue
		}
		if sr.q.Pattern != tc.re || sr.q.Re.String() != tc.re || sr.q.Limit != 1000 {
			t.Errorf("%s: pattern %q limit %d, want %q", tc.query, sr.q.Pattern, sr.q.Limit, tc.re)
		}
	}

	v, _ := url.ParseQuery("pattern=ab&type=dir&uid=33&newer=2026-09-01&fs=vol1,vol2")
	sr, _ := parseSearch(v, 10)
	want, _ := time.ParseInLocation("2006-01-02", "2026-09-01", time.Local)
	if sr.q.Type != 'd' || sr.q.UID != 33 || sr.q.Mtime != want.Unix() || strings.Join(sr.fs, ",") != "vol1,vol2" {
		t.Errorf("filters: %+v fs=%v", sr.q, sr.fs)
	}
	v, _ = url.ParseQuery("pattern=ab")
	if sr, _ = parseSearch(v, 10); sr.q.UID != -1 || sr.q.Mtime != 0 || sr.q.Type != 0 || sr.fs != nil {
		t.Errorf("defaults: %+v fs=%v", sr.q, sr.fs)
	}
}

func TestStaticAndHeaders(t *testing.T) {
	s := &server{}
	h := s.routes(os.DirFS("static"))
	for _, p := range []string{"/", "/app.js", "/app.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s: %d", p, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s: no CSP", p)
		}
	}
	for _, u := range []string{
		"/api/search?pattern=.*&regex=1",
		"/api/search?pattern=.*&match=regex",
		"/api/export?pattern=.*&match=regex",
		"/api/export?pattern=ab&format=xlsx",
		"/api/export?pattern=a&match=contains",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: %d %s", u, rec.Code, rec.Body)
		}
	}
}
