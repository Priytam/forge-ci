package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// azureStore keeps artifacts, caches and archived logs in an Azure Blob
// Storage container. Configure with ARTIFACT_STORE=azure and either
//
//	AZURE_STORAGE_CONNECTION_STRING          (account key or SAS inside), or
//	AZURE_STORAGE_ACCOUNT + AZURE_STORAGE_KEY (shared key), or
//	AZURE_STORAGE_ACCOUNT alone: the default Azure credential chain (managed
//	identity, workload identity, az login, env vars).
//
// AZURE_STORAGE_CONTAINER names the container (default "forge"); it is
// created when missing. AZURE_STORAGE_ENDPOINT overrides the service URL
// (Azurite: http://127.0.0.1:10000/<account>).
type azureStore struct {
	client    *azblob.Client
	container string
}

func newAzureFromEnv() (Store, error) {
	container := envOr("AZURE_STORAGE_CONTAINER", "forge")
	var client *azblob.Client
	var err error
	switch {
	case os.Getenv("AZURE_STORAGE_CONNECTION_STRING") != "":
		client, err = azblob.NewClientFromConnectionString(os.Getenv("AZURE_STORAGE_CONNECTION_STRING"), nil)
	case os.Getenv("AZURE_STORAGE_ACCOUNT") != "":
		account := os.Getenv("AZURE_STORAGE_ACCOUNT")
		endpoint := envOr("AZURE_STORAGE_ENDPOINT", fmt.Sprintf("https://%s.blob.core.windows.net/", account))
		if key := os.Getenv("AZURE_STORAGE_KEY"); key != "" {
			var cred *azblob.SharedKeyCredential
			cred, err = azblob.NewSharedKeyCredential(account, key)
			if err == nil {
				client, err = azblob.NewClientWithSharedKeyCredential(endpoint, cred, nil)
			}
		} else {
			var cred azcore.TokenCredential
			cred, err = azidentity.NewDefaultAzureCredential(nil)
			if err == nil {
				client, err = azblob.NewClient(endpoint, cred, nil)
			}
		}
	default:
		return nil, errors.New("ARTIFACT_STORE=azure requires AZURE_STORAGE_CONNECTION_STRING, or AZURE_STORAGE_ACCOUNT (+ AZURE_STORAGE_KEY, or a managed/workload identity)")
	}
	if err != nil {
		return nil, fmt.Errorf("azure blob client: %w", err)
	}
	s := &azureStore{client: client, container: container}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := client.CreateContainer(ctx, container, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		return nil, fmt.Errorf("azure blob container %q: %w", container, err)
	}
	return s, nil
}

func (a *azureStore) Kind() string { return "azure" }

func (a *azureStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	cr := &countingReader{r: r}
	ct := "application/gzip"
	_, err := a.client.UploadStream(ctx, a.container, key, cr, &azblob.UploadStreamOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &ct},
	})
	if err != nil {
		return 0, err
	}
	return cr.n, nil
}

func (a *azureStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := a.client.DownloadStream(ctx, a.container, key, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return resp.Body, nil
}

func (a *azureStore) Delete(ctx context.Context, key string) error {
	_, err := a.client.DeleteBlob(ctx, a.container, key, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return err
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
