package main

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var oidUID = asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}

func TestSubjectDN(t *testing.T) {
	// A client certificate subject, as openssl -nameopt RFC2253 shows
	// it: UID=1000,CN=alice,O=Example Org.
	name := pkix.Name{
		Organization: []string{"Example Org"},
		CommonName:   "alice",
		ExtraNames:   []pkix.AttributeTypeAndValue{{Type: oidUID, Value: "1000"}},
	}
	raw, err := asn1.Marshal(name.ToRDNSequence())
	if err != nil {
		t.Fatal(err)
	}
	got, err := subjectDN(raw)
	if want := "UID=1000,CN=alice,O=Example Org"; err != nil || got != want {
		t.Errorf("subjectDN = %q, %v; want %q", got, err, want)
	}
	// pkix.Name.String() is why subjectDN exists.
	if s := name.String(); strings.Contains(s, "UID=") {
		t.Logf("pkix.Name.String() now names UID: %s", s)
	}

	// Escaping and multi-valued RDNs.
	seq := pkix.RDNSequence{
		{{Type: asn1.ObjectIdentifier{2, 5, 4, 10}, Value: "Acme, Inc."}},
		{{Type: asn1.ObjectIdentifier{2, 5, 4, 3}, Value: " x+y "}, {Type: oidUID, Value: "7"}},
	}
	raw, _ = asn1.Marshal(seq)
	got, _ = subjectDN(raw)
	if want := `CN=\ x\+y\ +UID=7,O=Acme\, Inc.`; got != want {
		t.Errorf("escaped: %q, want %q", got, want)
	}
	if c, err := canonicalDN(got); err != nil || c != got {
		t.Errorf("canonicalDN does not round-trip %q: %q %v", got, c, err)
	}
}

func TestCanonicalDN(t *testing.T) {
	const want = "UID=1000,CN=alice,O=Example Org"
	for _, in := range []string{
		want,
		"uid=1000, cn=alice, o=Example Org",
		"UID = 1000 ; CN = alice ; O = Example Org",
		`UID=1000,CN=alice,O=Example\20Org`, // hex escape
	} {
		if got, err := canonicalDN(in); err != nil || got != want {
			t.Errorf("canonicalDN(%q) = %q, %v", in, got, err)
		}
	}
	// E=, openssl's emailAddress= and OID forms are the same attribute.
	const withMail = "EMAILADDRESS=a@b.c,UID=1,CN=x"
	for _, in := range []string{"E=a@b.c,UID=1,CN=x", "emailAddress=a@b.c,userid=1,CN=x", "1.2.840.113549.1.9.1=a@b.c,0.9.2342.19200300.100.1.1=1,2.5.4.3=x"} {
		if got, err := canonicalDN(in); err != nil || got != withMail {
			t.Errorf("canonicalDN(%q) = %q, %v; want %q", in, got, err, withMail)
		}
	}
	// Values stay case-sensitive.
	if got, _ := canonicalDN("CN=Alice"); got != "CN=Alice" {
		t.Errorf("value case changed: %q", got)
	}
	for _, bad := range []string{"alice", "CN=", "=x", "CN=a,,O=b"} {
		if got, err := canonicalDN(bad); err == nil {
			t.Errorf("canonicalDN(%q) = %q, want error", bad, got)
		}
	}
}

func TestReadSubjects(t *testing.T) {
	p := filepath.Join(t.TempDir(), "allowed")
	os.WriteFile(p, []byte("# team\n\nUID=1000, CN=alice, O=Example Org\n"), 0o644)
	got, err := readSubjects(p)
	if err != nil || len(got) != 1 || !got["UID=1000,CN=alice,O=Example Org"] {
		t.Errorf("readSubjects = %v, %v", got, err)
	}
	// A bare CN is rejected with its line number.
	os.WriteFile(p, []byte("# team\nalice\n"), 0o644)
	if _, err := readSubjects(p); err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Errorf("bare CN: err = %v", err)
	}
}

// The certificate a real handshake yields must format like the test above.
func TestSubjectDNFromCert(t *testing.T) {
	ca := newCA(t, "test CA")
	tc, _, _ := ca.issue(t, "alice", false)
	c, _ := x509.ParseCertificate(tc.Certificate[0])
	if got, err := subjectDN(c.RawSubject); err != nil || got != "CN=alice" {
		t.Errorf("subjectDN = %q, %v", got, err)
	}
}
