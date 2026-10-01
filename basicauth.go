package main

// HTTP basic auth against an htpasswd file, for setups without a PKI for
// client certificates (auth = basic). Only bcrypt entries ("htpasswd -B")
// are accepted. The connection is still HTTPS, so the password never goes
// over the wire in clear text.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// readHtpasswd reads "user:bcrypt-hash" lines. Blank lines and lines
// starting with # are ignored. Any other hash format is an error, so a
// file written with htpasswd's MD5 or SHA-1 default fails loudly.
func readHtpasswd(path string) (map[string][]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("htpasswd: %w", err)
	}
	out := map[string][]byte{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		user, hash, ok := strings.Cut(line, ":")
		if !ok || user == "" {
			return nil, fmt.Errorf("%s:%d: want user:hash", path, n)
		}
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return nil, fmt.Errorf("%s:%d: user %q: not a bcrypt hash (create entries with htpasswd -B): %v", path, n, user, err)
		}
		if _, dup := out[user]; dup {
			return nil, fmt.Errorf("%s:%d: user %q listed twice", path, n, user)
		}
		out[user] = []byte(hash)
	}
	return out, sc.Err()
}

// dummyHash returns a hash to compare against for unknown users, at the
// highest cost in users, so a login attempt takes about as long whether or
// not the user exists. htpasswd -B defaults to a lower cost than
// bcrypt.DefaultCost, so a fixed cost would give unknown users away.
func dummyHash(users map[string][]byte) ([]byte, error) {
	cost := bcrypt.MinCost
	for _, h := range users {
		if c, err := bcrypt.Cost(h); err == nil && c > cost {
			cost = c
		}
	}
	return bcrypt.GenerateFromPassword([]byte("cephfs-fe dummy password"), cost)
}

// checkPassword verifies user and password. The page makes several API
// calls per search, so a successful bcrypt check is remembered (as a
// SHA-256 of the password) until the htpasswd file is reloaded.
func (cm *certManager) checkPassword(user, pass string) bool {
	sum := sha256.Sum256([]byte(pass))
	cm.mu.RLock()
	hash, known := cm.users[user]
	cached, hit := cm.verified[user]
	dummy := cm.dummy
	cm.mu.RUnlock()
	if !known {
		_ = bcrypt.CompareHashAndPassword(dummy, []byte(pass))
		return false
	}
	if hit && subtle.ConstantTimeCompare(cached[:], sum[:]) == 1 {
		return true
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(pass)) != nil {
		return false
	}
	cm.mu.Lock()
	// Only cache against the hash that was checked: a reload in between
	// replaced cm.users and cleared the cache.
	if bytes.Equal(cm.users[user], hash) {
		cm.verified[user] = sum
	}
	cm.mu.Unlock()
	return true
}

// requireBasicAuth lets a request through only with a valid user and
// password from the htpasswd file.
func (cm *certManager) requireBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !cm.checkPassword(user, pass) {
			if ok {
				log.Printf("denied user %q from %s", user, r.RemoteAddr)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="CephFS Search", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, "user="+user)))
	})
}
