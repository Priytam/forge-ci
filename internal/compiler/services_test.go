package compiler

import "testing"

func compileOne(t *testing.T, yml string) CompiledJob {
	t.Helper()
	jobs, err := Compile(yml, "main", "push", nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	return jobs[0]
}

func TestServicesParsingLongAndShort(t *testing.T) {
	yml := `
stages: [test]
jobs:
  it:
    stage: test
    image: postgres:16-alpine
    services:
      - image: postgres:16-alpine
        alias: db
        env: {POSTGRES_PASSWORD: pw}
        cmd: ["postgres", "-c", "max_connections=50"]
      - redis:7
    script: [true]
`
	j := compileOne(t, yml)
	if len(j.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(j.Services))
	}
	pg := j.Services[0]
	if pg.Image != "postgres:16-alpine" || pg.Alias != "db" {
		t.Errorf("postgres service wrong: %+v", pg)
	}
	if pg.Env["POSTGRES_PASSWORD"] != "pw" {
		t.Errorf("expected env POSTGRES_PASSWORD=pw, got %v", pg.Env)
	}
	if len(pg.Cmd) != 3 || pg.Cmd[0] != "postgres" {
		t.Errorf("expected cmd override, got %v", pg.Cmd)
	}
	// Shorthand scalar: image only, alias derived from image name.
	rd := j.Services[1]
	if rd.Image != "redis:7" || rd.Alias != "redis" {
		t.Errorf("redis service wrong (alias should default to 'redis'): %+v", rd)
	}
}

func TestServicesDefaultAliasFromImage(t *testing.T) {
	cases := map[string]string{
		"postgres:16-alpine":            "postgres",
		"redis:7":                       "redis",
		"docker.io/library/mysql:8.0":   "mysql",
		"registry.io/team/my_svc:1.2.3": "my-svc",
	}
	for image, want := range cases {
		if got := defaultAlias(image); got != want {
			t.Errorf("defaultAlias(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestServicesValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		yml  string
	}{
		{"missing image", `
stages: [test]
jobs:
  it:
    stage: test
    services:
      - alias: db
    script: [true]
`},
		{"duplicate alias", `
stages: [test]
jobs:
  it:
    stage: test
    services:
      - {image: postgres:16, alias: db}
      - {image: mysql:8, alias: db}
    script: [true]
`},
		{"too many services", `
stages: [test]
jobs:
  it:
    stage: test
    services:
      - a:1
      - b:1
      - c:1
      - d:1
      - e:1
      - f:1
    script: [true]
`},
		{"invalid alias", `
stages: [test]
jobs:
  it:
    stage: test
    services:
      - {image: postgres:16, alias: "Bad_Alias"}
    script: [true]
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile(tc.yml, "main", "push", nil); err == nil {
				t.Errorf("expected error for %s, got none", tc.name)
			}
		})
	}
}

func TestServicesNoneMeansNil(t *testing.T) {
	yml := `
stages: [test]
jobs:
  it:
    stage: test
    script: [true]
`
	j := compileOne(t, yml)
	if j.Services != nil {
		t.Errorf("expected nil services when none declared, got %v", j.Services)
	}
}

func TestServicesSurviveExtendsMerge(t *testing.T) {
	// A hidden base declares services; the child inherits them (child sets none).
	yml := `
stages: [test]
jobs:
  .with-db:
    services:
      - {image: postgres:16-alpine, alias: db, env: {POSTGRES_PASSWORD: pw}}
  it:
    stage: test
    extends: .with-db
    image: postgres:16-alpine
    script: [true]
`
	j := compileOne(t, yml)
	if len(j.Services) != 1 || j.Services[0].Alias != "db" {
		t.Fatalf("expected inherited db service, got %+v", j.Services)
	}
	if j.Services[0].Env["POSTGRES_PASSWORD"] != "pw" {
		t.Errorf("expected inherited env, got %v", j.Services[0].Env)
	}
}

func TestServicesChildOverridesBaseInMerge(t *testing.T) {
	// Child services: replaces the base's wholesale (arrays are not element-merged).
	yml := `
stages: [test]
jobs:
  .with-db:
    services:
      - {image: postgres:16-alpine, alias: db}
  it:
    stage: test
    extends: .with-db
    services:
      - {image: redis:7, alias: cache}
    script: [true]
`
	j := compileOne(t, yml)
	if len(j.Services) != 1 || j.Services[0].Alias != "cache" {
		t.Fatalf("expected child to replace base services, got %+v", j.Services)
	}
}

func TestServicesSurviveMatrix(t *testing.T) {
	yml := `
stages: [test]
jobs:
  it:
    stage: test
    image: postgres:16-alpine
    services:
      - {image: postgres:16-alpine, alias: db}
    parallel:
      matrix:
        - PGVER: ["15", "16"]
    script: [true]
`
	jobs, err := Compile(yml, "main", "push", nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 matrix instances, got %d", len(jobs))
	}
	for _, j := range jobs {
		if len(j.Services) != 1 || j.Services[0].Alias != "db" {
			t.Errorf("matrix instance %q missing db service: %+v", j.Name, j.Services)
		}
	}
}
