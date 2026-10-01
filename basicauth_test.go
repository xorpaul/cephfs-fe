package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func htpasswdLine(t *testing.T, user, pass string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return user + ":" + string(h) + "\n"
}

func TestReadHtpasswd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "htpasswd")
	write(t, p, []byte("# team\n\n"+htpasswdLine(t, "alice", "secret")))
	got, err := readHtpasswd(p)
	if err != nil || len(got) != 1 || got["alice"] == nil {
		t.Errorf("readHtpasswd = %v, %v", got, err)
	}
	for name, content := range map[string]string{
		"md5":       "alice:$apr1$abcdefgh$0123456789abcdefghijkl\n",
		"sha1":      "alice:{SHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=\n",
		"no colon":  "alice\n",
		"no user":   ":$2y$05$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz01234\n",
		"duplicate": htpasswdLine(t, "alice", "a") + htpasswdLine(t, "alice", "b"),
	} {
		write(t, p, []byte(content))
		if _, err := readHtpasswd(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDummyHashCost(t *testing.T) {
	users := map[string][]byte{}
	for user, cost := range map[string]int{"alice": 5, "bob": 6} {
		h, _ := bcrypt.GenerateFromPassword([]byte("x"), cost)
		users[user] = h
	}
	d, err := dummyHash(users)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := bcrypt.Cost(d); c != 6 {
		t.Errorf("dummy cost = %d, want the highest user cost 6", c)
	}
}

func TestBasicAuth(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "test CA")
	_, srvCert, srvKey := ca.issue(t, "server", true)
	write(t, filepath.Join(dir, "srv.pem"), srvCert)
	write(t, filepath.Join(dir, "srv.key"), srvKey)
	users := filepath.Join(dir, "htpasswd")
	write(t, users, []byte(htpasswdLine(t, "alice", "s3cret")+htpasswdLine(t, "bob", "hunter2")))

	cm, err := newCertManager(filepath.Join(dir, "srv.pem"), filepath.Join(dir, "srv.key"), "", "", users)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: cm.requireBasicAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "hello "+clientID(r))
		})),
		TLSConfig: cm.tlsConfig(),
		ErrorLog:  log.New(io.Discard, "", 0),
	}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	get := func(user, pass string) (int, string, http.Header) {
		req, _ := http.NewRequest("GET", "https://"+ln.Addr().String(), nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b), r.Header
	}

	if code, _, h := get("", ""); code != http.StatusUnauthorized || !strings.HasPrefix(h.Get("WWW-Authenticate"), "Basic ") {
		t.Errorf("no credentials: %d %q", code, h.Get("WWW-Authenticate"))
	}
	for _, bad := range [][2]string{{"alice", "wrong"}, {"mallory", "s3cret"}, {"alice", ""}} {
		if code, _, _ := get(bad[0], bad[1]); code != http.StatusUnauthorized {
			t.Errorf("%s/%s: %d", bad[0], bad[1], code)
		}
	}
	// Twice: the second request is answered from the verified cache.
	for range 2 {
		if code, body, _ := get("alice", "s3cret"); code != http.StatusOK || body != "hello user=alice" {
			t.Errorf("alice: %d %q", code, body)
		}
	}
	// A wrong password must not match the cached one.
	if code, _, _ := get("alice", "s3cret!"); code != http.StatusUnauthorized {
		t.Errorf("alice with another password after a cached login: %d", code)
	}

	// Remove alice and change bob's password: the reload clears the cache.
	write(t, users, []byte(htpasswdLine(t, "bob", "correct horse")))
	os.Chtimes(users, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if !cm.changed() {
		t.Fatal("htpasswd change not detected")
	}
	if err := cm.reload(); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := get("alice", "s3cret"); code != http.StatusUnauthorized {
		t.Errorf("removed alice: %d", code)
	}
	if code, _, _ := get("bob", "hunter2"); code != http.StatusUnauthorized {
		t.Errorf("bob with old password: %d", code)
	}
	if code, body, _ := get("bob", "correct horse"); code != http.StatusOK || body != "hello user=bob" {
		t.Errorf("bob with new password: %d %q", code, body)
	}

	// A broken file keeps the previous users.
	write(t, users, []byte("bob:{SHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=\n"))
	if err := cm.reload(); err == nil {
		t.Error("reload with a non-bcrypt entry succeeded")
	}
	if code, _, _ := get("bob", "correct horse"); code != http.StatusOK {
		t.Errorf("bob after failed reload: %d", code)
	}

	// No client certificate is requested in basic mode.
	if cfg, _ := cm.tlsConfig().GetConfigForClient(nil); cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert", cfg.ClientAuth)
	}
}
