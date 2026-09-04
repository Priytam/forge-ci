package vcs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"
)

var (
	testSignRepo  = CodeCommitRepo{Region: "ap-south-1", Name: "tablespace-api"}
	testSignAt    = time.Date(2026, 9, 4, 10, 30, 0, 0, time.UTC)
	testSignCreds = AWSCredentials{
		AccessKeyID:     "ASIAEXAMPLEACCESSKEY",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		SessionToken:    "FwoGZXIvYXdzEBY//////////wEaDExample+Token/With=Padding",
	}
)

// The signature is checked against an INDEPENDENT re-derivation of
// git-remote-codecommit's scheme, so the test would catch a change to the
// canonical request, the scope, or the HMAC chain — not merely echo the
// implementation back.
func TestSignCloneURLMatchesGRCScheme(t *testing.T) {
	got, err := SignCloneURL(testSignRepo, testSignCreds, testSignAt)
	if err != nil {
		t.Fatal(err)
	}

	const (
		host = "git-codecommit.ap-south-1.amazonaws.com"
		path = "/v1/repos/tablespace-api"
		ts   = "20260904T103000"
	)
	canonical := "GIT\n" + path + "\n\nhost:" + host + "\n\nhost\n"
	csum := sha256.Sum256([]byte(canonical))
	stringToSign := "AWS4-HMAC-SHA256\n" + ts +
		"\n20260904/ap-south-1/codecommit/aws4_request\n" + hex.EncodeToString(csum[:])

	mac := func(key []byte, data string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(data))
		return m.Sum(nil)
	}
	k := mac([]byte("AWS4"+testSignCreds.SecretAccessKey), "20260904")
	k = mac(k, "ap-south-1")
	k = mac(k, "codecommit")
	k = mac(k, "aws4_request")
	wantPassword := ts + "Z" + hex.EncodeToString(mac(k, stringToSign))

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("signed URL does not parse: %v (%q)", err, got)
	}
	if u.Scheme != "https" || u.Host != host || u.Path != path {
		t.Errorf("URL target = %s://%s%s, want https://%s%s", u.Scheme, u.Host, u.Path, host, path)
	}
	password, _ := u.User.Password()
	if password != wantPassword {
		t.Errorf("password = %q, want %q", password, wantPassword)
	}
	// The username is access key + "%" + session token, percent-encoded whole.
	if u.User.Username() != testSignCreds.AccessKeyID+"%"+testSignCreds.SessionToken {
		t.Errorf("username = %q", u.User.Username())
	}
}

// Long-lived credentials carry no session token, so the username is the bare
// access key id with no "%" separator.
func TestSignCloneURLWithoutSessionToken(t *testing.T) {
	got, err := SignCloneURL(testSignRepo, AWSCredentials{
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret",
	}, testSignAt)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.User.Username() != "AKIAEXAMPLE" {
		t.Errorf("username = %q, want the bare access key id", u.User.Username())
	}
	if strings.Contains(u.User.Username(), "%") {
		t.Error("username has a session-token separator but there is no session token")
	}
}

// A session token contains '/', '+' and '=' — all of which would break the URL
// if they reached it raw.
func TestSignCloneURLEscapesSessionToken(t *testing.T) {
	got, err := SignCloneURL(testSignRepo, testSignCreds, testSignAt)
	if err != nil {
		t.Fatal(err)
	}
	userinfo := got[len("https://"):strings.Index(got, "@")]
	rawUser := userinfo[:strings.Index(userinfo, ":")]
	for _, bad := range []string{"/", "+", "="} {
		if strings.Contains(rawUser, bad) {
			t.Errorf("unescaped %q in the URL userinfo: %q", bad, rawUser)
		}
	}
}

// The signature must change with the clock, so a captured URL cannot be replayed
// indefinitely.
func TestSignCloneURLVariesWithTime(t *testing.T) {
	a, err := SignCloneURL(testSignRepo, testSignCreds, testSignAt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SignCloneURL(testSignRepo, testSignCreds, testSignAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("signature did not change with the timestamp")
	}
}

// A non-UTC clock must still sign in UTC, or the signature is rejected.
func TestSignCloneURLNormalisesToUTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	utc, err := SignCloneURL(testSignRepo, testSignCreds, testSignAt)
	if err != nil {
		t.Fatal(err)
	}
	local, err := SignCloneURL(testSignRepo, testSignCreds, testSignAt.In(ist))
	if err != nil {
		t.Fatal(err)
	}
	if utc != local {
		t.Error("signing the same instant in a different zone produced a different URL")
	}
}

func TestSignCloneURLRejectsIncompleteInput(t *testing.T) {
	tests := []struct {
		name  string
		repo  CodeCommitRepo
		creds AWSCredentials
	}{
		{"no region", CodeCommitRepo{Name: "r"}, testSignCreds},
		{"no repo name", CodeCommitRepo{Region: "ap-south-1"}, testSignCreds},
		{"no credentials", testSignRepo, AWSCredentials{}},
		{"no secret key", testSignRepo, AWSCredentials{AccessKeyID: "AKIA"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := SignCloneURL(tt.repo, tt.creds, testSignAt); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// The signed URL must never contain the secret access key itself — only a
// signature derived from it.
func TestSignCloneURLNeverContainsSecretKey(t *testing.T) {
	got, err := SignCloneURL(testSignRepo, testSignCreds, testSignAt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, testSignCreds.SecretAccessKey) {
		t.Error("the signed URL leaked the secret access key")
	}
	if strings.Contains(got, url.QueryEscape(testSignCreds.SecretAccessKey)) {
		t.Error("the signed URL leaked the secret access key (escaped)")
	}
}
