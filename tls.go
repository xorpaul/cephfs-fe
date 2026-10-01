package main

// Client-certificate auth: the handshake uses
// VerifyClientCertIfGiven so browsers without a certificate still get an
// explanatory HTML page instead of a raw TLS alert, and the middleware
// checks the certificate's full subject DN against an allow list. A certificate that is presented but
// does not chain to the CA still aborts the handshake.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// certManager holds the server certificate and the credentials clients are
// checked against, and reloads them when their files change: the client CA
// pool and allowed subject DNs (auth = mtls), or the htpasswd users
// (auth = basic, caFile and subjectsFile empty).
type certManager struct {
	certFile, keyFile, caFile, subjectsFile, htpasswdFile string
	// certHelpURL and accessHelpURL are linked from the 401 and 403 pages
	// when set: where to get a client certificate, and how to request access.
	certHelpURL, accessHelpURL string

	mu       sync.RWMutex
	cert     *tls.Certificate
	cas      *x509.CertPool
	allowed  map[string]bool
	users    map[string][]byte   // htpasswd user -> bcrypt hash
	verified map[string][32]byte // user -> SHA-256 of a password that passed bcrypt
	dummy    []byte              // bcrypt hash checked for unknown users
	mtimes   map[string]time.Time
}

// newCertManager loads the files. Pass caFile and subjectsFile for client
// certificate auth, or htpasswdFile for basic auth.
func newCertManager(certFile, keyFile, caFile, subjectsFile, htpasswdFile string) (*certManager, error) {
	cm := &certManager{certFile: certFile, keyFile: keyFile, caFile: caFile, subjectsFile: subjectsFile, htpasswdFile: htpasswdFile}
	if err := cm.reload(); err != nil {
		return nil, err
	}
	return cm, nil
}

// reload reads all files and swaps them in together; on error the previous
// state is kept.
func (cm *certManager) reload() error {
	mtimes := map[string]time.Time{}
	for _, f := range cm.files() {
		fi, err := os.Stat(f)
		if err != nil {
			return err
		}
		mtimes[f] = fi.ModTime()
	}
	cert, err := tls.LoadX509KeyPair(cm.certFile, cm.keyFile)
	if err != nil {
		return fmt.Errorf("server certificate: %w", err)
	}
	var cas *x509.CertPool
	var allowed map[string]bool
	if cm.caFile != "" {
		pem, err := os.ReadFile(cm.caFile)
		if err != nil {
			return fmt.Errorf("client CA: %w", err)
		}
		cas = x509.NewCertPool()
		if !cas.AppendCertsFromPEM(pem) {
			return fmt.Errorf("client CA %s: no certificates found", cm.caFile)
		}
		if allowed, err = readSubjects(cm.subjectsFile); err != nil {
			return err
		}
	}
	var users map[string][]byte
	var dummy []byte
	if cm.htpasswdFile != "" {
		if users, err = readHtpasswd(cm.htpasswdFile); err != nil {
			return err
		}
		if dummy, err = dummyHash(users); err != nil {
			return fmt.Errorf("htpasswd: %w", err)
		}
	}
	cm.mu.Lock()
	cm.cert, cm.cas, cm.allowed, cm.users, cm.dummy, cm.mtimes = &cert, cas, allowed, users, dummy, mtimes
	cm.verified = map[string][32]byte{}
	cm.mu.Unlock()
	if users != nil {
		log.Printf("loaded certificate and %d htpasswd users", len(users))
	} else {
		log.Printf("loaded certificates and %d allowed subjects", len(allowed))
	}
	return nil
}

func (cm *certManager) files() []string {
	var out []string
	for _, f := range []string{cm.certFile, cm.keyFile, cm.caFile, cm.subjectsFile, cm.htpasswdFile} {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// changed reports whether any file's mtime differs from the loaded one.
func (cm *certManager) changed() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	for _, f := range cm.files() {
		fi, err := os.Stat(f)
		if err == nil && !fi.ModTime().Equal(cm.mtimes[f]) {
			return true
		}
	}
	return false
}

// watch reloads when a file changes (polled) or a value arrives on hup.
func (cm *certManager) watch(ctx context.Context, every time.Duration, hup <-chan os.Signal) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		case <-t.C:
			if !cm.changed() {
				continue
			}
		}
		if err := cm.reload(); err != nil {
			log.Printf("reload failed, keeping previous certificates: %v", err)
		}
	}
}

// tlsConfig builds the listener config. GetConfigForClient returns the
// current certificate and CA pool per handshake, so a reloaded CA applies
// to new connections.
func (cm *certManager) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Unused once GetConfigForClient answers, but it makes ServeTLS("", "")
		// accept the config as having a certificate.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cm.mu.RLock()
			defer cm.mu.RUnlock()
			return cm.cert, nil
		},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cm.mu.RLock()
			defer cm.mu.RUnlock()
			cfg := &tls.Config{
				MinVersion:   tls.VersionTLS12,
				NextProtos:   []string{"h2", "http/1.1"}, // the returned config replaces the server's ALPN list
				Certificates: []tls.Certificate{*cm.cert},
			}
			if cm.cas != nil {
				cfg.ClientAuth, cfg.ClientCAs = tls.VerifyClientCertIfGiven, cm.cas
			}
			return cfg, nil
		},
	}
}

func (cm *certManager) isAllowed(dn string) bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.allowed[dn]
}

// readSubjects reads one subject DN per line, e.g.
// "UID=1000,CN=alice,O=Example Org". Blank lines and lines
// starting with # are ignored. Entries are normalized (see canonicalDN); an
// entry that is not a DN, such as a bare CN, is an error instead of silently
// locking its user out.
func readSubjects(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("allowed subjects: %w", err)
	}
	out := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		dn, err := canonicalDN(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: not a subject DN (want e.g. UID=…,CN=…,O=…): %v", path, n, err)
		}
		out[dn] = true
	}
	return out, sc.Err()
}

type ctxKey struct{}

// clientID returns the authenticated client: the subject DN stored by
// requireClientCert, or "user=<name>" from requireBasicAuth.
func clientID(r *http.Request) string {
	dn, _ := r.Context().Value(ctxKey{}).(string)
	return dn
}

// requireClientCert lets a request through only with a verified client
// certificate whose subject DN is on the allow list.
func (cm *certManager) requireClientCert(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			cm.writeCertErrorPage(w, "")
			return
		}
		dn, err := subjectDN(r.TLS.PeerCertificates[0].RawSubject)
		if err != nil {
			log.Printf("denied %s: unparsable subject: %v", r.RemoteAddr, err)
			http.Error(w, "unparsable certificate subject", http.StatusBadRequest)
			return
		}
		if !cm.isAllowed(dn) {
			log.Printf("denied %q from %s", dn, r.RemoteAddr)
			cm.writeCertErrorPage(w, dn)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, dn)))
	})
}

// writeCertErrorPage explains that a client certificate is needed and links
// to certHelpURL and accessHelpURL when they are set. dn is non-empty when a
// certificate was presented but its subject is not on the allow list.
func (cm *certManager) writeCertErrorPage(w http.ResponseWriter, dn string) {
	link := func(class, url, text string) string {
		if url == "" {
			return ""
		}
		u := html.EscapeString(url)
		return `<a class="` + class + `" href="` + u + `" target="_blank" rel="noopener">` + text + `</a>`
	}
	var title, body string
	if dn == "" {
		title = "Client Certificate Required"
		body = `<p>CephFS Search requires a personal client certificate.<br>
Your browser does not have a valid certificate installed for this site.</p>
` + link("btn", cm.certHelpURL, "Get your certificate &rarr;") + `
<p class="detail">After installing the certificate, reload this page.<br>
You may need to restart your browser for it to appear.</p>`
	} else {
		title = "Certificate Not Authorised"
		body = `<p>Your certificate is not on the allow&nbsp;list for CephFS Search. Its subject is:</p>
<p><code>` + html.EscapeString(dn) + `</code></p>
<p>Include this subject when you request access.</p>
` + link("btn", cm.accessHelpURL, "Request access &rarr;")
		if cm.certHelpURL != "" {
			body += `
<p class="detail">Need a certificate first? ` + link("", cm.certHelpURL, html.EscapeString(cm.certHelpURL)) + `</p>`
		}
	}

	page := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + title + ` — CephFS Search</title>
<style>
*{box-sizing:border-box}
body{font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;
     display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;padding:1rem}
.card{background:#1e293b;border:1px solid #334155;border-radius:12px;
      padding:2.5rem 3rem;max-width:500px;width:100%;text-align:center}
.icon{font-size:3rem;margin-bottom:1rem}
h1{color:#f8fafc;font-size:1.4rem;margin:0 0 1rem}
p{color:#94a3b8;line-height:1.65;margin:0 0 1.25rem}
code{background:#0f172a;border-radius:4px;padding:2px 6px;font-size:0.9em;color:#7dd3fc}
a.btn{display:inline-block;background:#3b82f6;color:#fff;text-decoration:none;
      padding:0.65rem 1.6rem;border-radius:7px;font-weight:600;font-size:0.95rem;margin-bottom:1.25rem}
a.btn:hover{background:#2563eb}
.detail{font-size:0.8rem;color:#64748b;margin-top:0.5rem}
.detail a{color:#60a5fa}
</style>
</head>
<body>
<div class="card">
<div class="icon">&#128274;</div>
<h1>` + title + `</h1>
` + body + `
</div>
</body>
</html>`

	status := http.StatusUnauthorized
	if dn != "" {
		status = http.StatusForbidden
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprint(w, page)
}
