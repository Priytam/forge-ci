package store

import "testing"

func TestParseDotenv(t *testing.T) {
	got, err := ParseDotenv("# build outputs\nDEPLOY_VERSION=1.2.3\nexport IMAGE='reg/app@sha256:ab'\n\nNOTE=\"a = b\"\n")
	if err != nil || got["DEPLOY_VERSION"] != "1.2.3" || got["IMAGE"] != "reg/app@sha256:ab" || got["NOTE"] != "a = b" {
		t.Fatalf("parse = %v %v", got, err)
	}
	for _, bad := range []string{"no equals sign", "1BAD=x", "CI_COMMIT_SHA=spoofed", "FORGE_OIDC_TOKEN=x"} {
		if _, err := ParseDotenv(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseCobertura(t *testing.T) {
	pct, err := ParseCobertura(`<?xml version="1.0"?><coverage line-rate="0.8312" branch-rate="0.5" version="1"><packages/></coverage>`)
	if err != nil || pct != 83.12 {
		t.Errorf("cobertura = %v %v", pct, err)
	}
	if _, err := ParseCobertura("<testsuite/>"); err == nil {
		t.Error("non-cobertura accepted")
	}
}

func TestDeployFacts(t *testing.T) {
	url, version, digest := deployFacts("https://$APP.example.com", map[string]string{"APP": "shop"}, "0123456789abcdef")
	if url != "https://shop.example.com" || version != "01234567" || digest != "" {
		t.Errorf("from yaml = %s %s %s", url, version, digest)
	}
	url, version, digest = deployFacts("https://ignored", map[string]string{"DEPLOY_URL": "https://live.example.com", "DEPLOY_VERSION": "v9", "DEPLOY_DIGEST": "sha256:ff"}, "abc")
	if url != "https://live.example.com" || version != "v9" || digest != "sha256:ff" {
		t.Errorf("from dotenv = %s %s %s", url, version, digest)
	}
	if url, _, _ := deployFacts("javascript:alert(1)", nil, "abc"); url != "" {
		t.Errorf("non-web URL kept: %s", url)
	}
}
