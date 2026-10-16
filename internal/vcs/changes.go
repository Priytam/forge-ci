package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
)

// CompareRequest asks for the files changed between two commits (or refs).
// Base may be a branch name; GitHub and Bitbucket then diff from the merge
// base of Base and Head ("what this branch changed"), CodeCommit diffs the two
// commits directly.
type CompareRequest struct {
	Provider string
	Repo     string
	Base     string
	Head     string
	Token    string

	CloneURL string
	Region   string
	Profile  string
	RoleARN  string
}

// maxChangedFiles: beyond this the list is reported incomplete, and callers
// treat the change set as unknown (every rules: changes: matches).
const maxChangedFiles = 3000

// ChangedFiles returns the repo-relative paths changed between req.Base and
// req.Head (both sides of renames). complete is false when the provider
// truncated the list or the provider is unsupported; callers must then not
// skip anything on the basis of the list.
func (f *Fetcher) ChangedFiles(ctx context.Context, req CompareRequest) (files []string, complete bool, err error) {
	switch req.Provider {
	case "github":
		return f.githubChanged(ctx, req)
	case "bitbucket":
		return f.bitbucketChanged(ctx, req)
	case "codecommit":
		return f.codecommitChanged(ctx, req)
	}
	return nil, false, nil
}

func (f *Fetcher) getJSON(ctx context.Context, reqURL, token string, v any) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vcs: compare failed: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v)
}

// githubChanged uses the compare API (three-dot: from the merge base). GitHub
// returns at most 300 files in one compare; more means incomplete.
func (f *Fetcher) githubChanged(ctx context.Context, req CompareRequest) ([]string, bool, error) {
	owner, name, ok := splitRepo(req.Repo)
	if !ok {
		return nil, false, fmt.Errorf("invalid repo %q", req.Repo)
	}
	var out struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", f.githubBase, owner, name,
		url.PathEscape(req.Base), url.PathEscape(req.Head))
	if err := f.getJSON(ctx, u, req.Token, &out); err != nil {
		return nil, false, err
	}
	var files []string
	for _, fl := range out.Files {
		files = append(files, fl.Filename)
		if fl.PreviousFilename != "" {
			files = append(files, fl.PreviousFilename)
		}
	}
	return files, len(out.Files) < 300, nil
}

// bitbucketChanged uses diffstat (head..base, merge-base aware), following
// pagination up to maxChangedFiles.
func (f *Fetcher) bitbucketChanged(ctx context.Context, req CompareRequest) ([]string, bool, error) {
	owner, name, ok := splitRepo(req.Repo)
	if !ok {
		return nil, false, fmt.Errorf("invalid repo %q", req.Repo)
	}
	next := fmt.Sprintf("%s/2.0/repositories/%s/%s/diffstat/%s..%s?pagelen=500", f.bitbucketBase, owner, name,
		url.PathEscape(req.Head), url.PathEscape(req.Base))
	var files []string
	for next != "" {
		var page struct {
			Values []struct {
				Old *struct {
					Path string `json:"path"`
				} `json:"old"`
				New *struct {
					Path string `json:"path"`
				} `json:"new"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if err := f.getJSON(ctx, next, req.Token, &page); err != nil {
			return nil, false, err
		}
		for _, v := range page.Values {
			if v.New != nil {
				files = append(files, v.New.Path)
			}
			if v.Old != nil && (v.New == nil || v.Old.Path != v.New.Path) {
				files = append(files, v.Old.Path)
			}
		}
		if len(files) > maxChangedFiles {
			return files, false, nil
		}
		next = page.Next
	}
	return files, true, nil
}

// codecommitChanged uses GetDifferences between the two commit specifiers.
func (f *Fetcher) codecommitChanged(ctx context.Context, req CompareRequest) ([]string, bool, error) {
	repo, ok := ParseCodeCommitURL(req.CloneURL)
	if req.Region != "" {
		repo.Region = req.Region
		ok = ok || repo.Name != ""
	}
	if req.Profile != "" {
		repo.Profile = req.Profile
	}
	if !ok || repo.Region == "" || repo.Name == "" {
		return nil, false, fmt.Errorf("invalid codecommit clone URL %q", req.CloneURL)
	}
	cl, err := f.cc.client(ctx, repo.Region, req.RoleARN, repo.Profile)
	if err != nil {
		return nil, false, err
	}
	var files []string
	var token *string
	for {
		out, err := cl.GetDifferences(ctx, &codecommit.GetDifferencesInput{
			RepositoryName:        aws.String(repo.Name),
			BeforeCommitSpecifier: aws.String(req.Base),
			AfterCommitSpecifier:  aws.String(req.Head),
			NextToken:             token,
		})
		if err != nil {
			return nil, false, fmt.Errorf("vcs: codecommit GetDifferences: %s", awsErrCode(err))
		}
		for _, d := range out.Differences {
			if d.AfterBlob != nil && d.AfterBlob.Path != nil {
				files = append(files, *d.AfterBlob.Path)
			}
			if d.BeforeBlob != nil && d.BeforeBlob.Path != nil &&
				(d.AfterBlob == nil || d.AfterBlob.Path == nil || *d.AfterBlob.Path != *d.BeforeBlob.Path) {
				files = append(files, *d.BeforeBlob.Path)
			}
		}
		if len(files) > maxChangedFiles {
			return files, false, nil
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return files, true, nil
		}
		token = out.NextToken
	}
}
