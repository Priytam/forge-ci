package compiler

import "testing"

func TestCacheLiteralKey(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
    cache:
      key: v1-deps
      paths: [vendor/, .cache/go]
`
	jobs, err := Compile(yml, "main", "push", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := jobNames(jobs)["build"]
	if j.CacheKey != "v1-deps" {
		t.Errorf("CacheKey = %q, want v1-deps", j.CacheKey)
	}
	if len(j.CacheKeyFiles) != 0 {
		t.Errorf("CacheKeyFiles = %v, want empty", j.CacheKeyFiles)
	}
	if j.CachePolicy != "pull-push" {
		t.Errorf("CachePolicy = %q, want pull-push (default)", j.CachePolicy)
	}
	if len(j.CachePaths) != 2 || j.CachePaths[0] != "vendor/" {
		t.Errorf("CachePaths = %v", j.CachePaths)
	}
}

func TestCacheFilesKey(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
    cache:
      key:
        files: [go.sum, go.mod]
        prefix: v2
      paths: [vendor/]
      policy: pull
`
	jobs, err := Compile(yml, "main", "push", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := jobNames(jobs)["build"]
	if j.CacheKey != "v2" {
		t.Errorf("CacheKey (prefix) = %q, want v2", j.CacheKey)
	}
	if len(j.CacheKeyFiles) != 2 || j.CacheKeyFiles[0] != "go.sum" {
		t.Errorf("CacheKeyFiles = %v", j.CacheKeyFiles)
	}
	if j.CachePolicy != "pull" {
		t.Errorf("CachePolicy = %q, want pull", j.CachePolicy)
	}
}

func TestCacheNoCacheWhenAbsent(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
`
	jobs, err := Compile(yml, "main", "push", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := jobNames(jobs)["build"]
	if len(j.CachePaths) != 0 || j.CachePolicy != "" || j.CacheKey != "" {
		t.Errorf("expected no cache, got paths=%v policy=%q key=%q", j.CachePaths, j.CachePolicy, j.CacheKey)
	}
}

func TestCacheRejectsPathsWithoutKey(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
    cache:
      paths: [vendor/]
`
	if _, err := Compile(yml, "main", "push", nil); err == nil {
		t.Fatal("expected error for cache with paths but no key")
	}
}

func TestCacheRejectsBadPolicy(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
    cache:
      key: k
      paths: [vendor/]
      policy: bogus
`
	if _, err := Compile(yml, "main", "push", nil); err == nil {
		t.Fatal("expected error for invalid cache policy")
	}
}

func TestCacheRejectsEmptyFilesList(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
    cache:
      key:
        files: []
      paths: [vendor/]
`
	if _, err := Compile(yml, "main", "push", nil); err == nil {
		t.Fatal("expected error for empty files list")
	}
}

func TestCacheSurvivesExtends(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  .base:
    stage: build
    script: [make]
    cache:
      key: base-key
      paths: [vendor/]
  build:
    extends: .base
`
	jobs, err := Compile(yml, "main", "push", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := jobNames(jobs)["build"]
	if j.CacheKey != "base-key" || len(j.CachePaths) != 1 {
		t.Errorf("cache not inherited via extends: key=%q paths=%v", j.CacheKey, j.CachePaths)
	}
	if j.CachePolicy != "pull-push" {
		t.Errorf("inherited cache policy = %q, want pull-push", j.CachePolicy)
	}
}

func TestCacheExtendsChildOverride(t *testing.T) {
	const yml = `
stages: [build]
jobs:
  .base:
    stage: build
    script: [make]
    cache:
      key: base-key
      paths: [vendor/]
  build:
    extends: .base
    cache:
      key: child-key
      paths: [node_modules/]
      policy: push
`
	jobs, err := Compile(yml, "main", "push", nil)
	if err != nil {
		t.Fatal(err)
	}
	j := jobNames(jobs)["build"]
	if j.CacheKey != "child-key" || j.CachePolicy != "push" {
		t.Errorf("child cache override failed: key=%q policy=%q", j.CacheKey, j.CachePolicy)
	}
	if len(j.CachePaths) != 1 || j.CachePaths[0] != "node_modules/" {
		t.Errorf("child cache paths = %v", j.CachePaths)
	}
}
