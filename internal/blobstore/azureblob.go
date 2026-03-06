package blobstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/appendblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"hivemq-canary/internal/config"
)

// AzureBlobWriter implements BlobWriter using the Azure Blob Storage SDK.
type AzureBlobWriter struct {
	containerClient *container.Client
}

// NewAzureBlobWriter creates a BlobWriter backed by Azure Blob Storage.
// Uses shared key auth if AccountKey is set, otherwise falls back to DefaultAzureCredential.
func NewAzureBlobWriter(cfg config.AzureBlobConfig) (*AzureBlobWriter, error) {
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net", cfg.AccountName)

	var containerClient *container.Client

	if cfg.AccountKey != "" {
		cred, err := azblob.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("creating shared key credential: %w", err)
		}
		client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("creating blob client: %w", err)
		}
		containerClient = client.ServiceClient().NewContainerClient(cfg.Container)
	} else {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("creating default credential: %w", err)
		}
		client, err := azblob.NewClient(serviceURL, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("creating blob client: %w", err)
		}
		containerClient = client.ServiceClient().NewContainerClient(cfg.Container)
	}

	return &AzureBlobWriter{containerClient: containerClient}, nil
}

func (w *AzureBlobWriter) CreateIfNotExists(ctx context.Context, blobName string) error {
	appendClient := w.containerClient.NewAppendBlobClient(blobName)
	_, err := appendClient.Create(ctx, nil)
	if err != nil && bloberror.HasCode(err, bloberror.BlobAlreadyExists) {
		return nil
	}
	return err
}

func (w *AzureBlobWriter) AppendBlock(ctx context.Context, blobName string, data []byte) error {
	appendClient := w.containerClient.NewAppendBlobClient(blobName)
	reader := bytes.NewReader(data)
	_, err := appendClient.AppendBlock(ctx, readSeekCloser{reader}, &appendblob.AppendBlockOptions{})
	if err != nil {
		return fmt.Errorf("append block: %w", err)
	}
	return nil
}

func (w *AzureBlobWriter) DeleteBlob(ctx context.Context, blobName string) error {
	blobClient := w.containerClient.NewBlobClient(blobName)
	_, err := blobClient.Delete(ctx, nil)
	return err
}

func (w *AzureBlobWriter) ListBlobs(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	pager := w.containerClient.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix: &prefix,
	})
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing blobs: %w", err)
		}
		for _, blob := range resp.Segment.BlobItems {
			if blob.Name != nil {
				names = append(names, *blob.Name)
			}
		}
	}
	return names, nil
}

// blobDateStr extracts a date string from a blob name like "metrics/2024-01-15.jsonl".
func blobDateStr(name string) string {
	name = strings.TrimPrefix(name, "metrics/")
	name = strings.TrimSuffix(name, ".jsonl")
	return name
}

// readSeekCloser wraps an io.ReadSeeker with a no-op Close method.
type readSeekCloser struct {
	io.ReadSeeker
}

func (readSeekCloser) Close() error { return nil }

// ensure AzureBlobWriter satisfies BlobWriter at compile time
var _ BlobWriter = (*AzureBlobWriter)(nil)

