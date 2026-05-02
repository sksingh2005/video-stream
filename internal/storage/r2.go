package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sksingh2005/video-stream/internal/config"
)

type R2Client struct {
	client *s3.Client
	bucket string
}

type UploadedObject struct {
	ObjectKey string
	Size      int64
}

func NewR2Client(cfg config.R2Config) (*R2Client, error) {
	client := s3.New(s3.Options{
		Region:                     "auto",
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		BaseEndpoint:               aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)),
		UsePathStyle:               true,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})

	return &R2Client{client: client, bucket: cfg.BucketName}, nil
}

func (c *R2Client) UploadFile(ctx context.Context, objectKey, filePath string, contentType string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file %s: %w", filePath, err)
	}
	defer file.Close()

	_, err = c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(strings.TrimLeft(objectKey, "/")),
		Body:        file,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", objectKey, err)
	}

	return nil
}

func (c *R2Client) UploadBytes(ctx context.Context, objectKey string, payload []byte, contentType string) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(strings.TrimLeft(objectKey, "/")),
		Body:          bytes.NewReader(payload),
		ContentLength: aws.Int64(int64(len(payload))),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", objectKey, err)
	}
	return nil
}

func (c *R2Client) UploadDir(ctx context.Context, localDir, remotePrefix string) error {
	_, err := c.UploadDirVerified(ctx, localDir, remotePrefix)
	return err
}

func (c *R2Client) UploadDirVerified(ctx context.Context, localDir, remotePrefix string) ([]UploadedObject, error) {
	uploaded := make([]UploadedObject, 0)
	rollbackRequired := false
	defer func() {
		if !rollbackRequired || len(uploaded) == 0 {
			return
		}

		objectKeys := make([]string, 0, len(uploaded))
		for _, item := range uploaded {
			objectKeys = append(objectKeys, item.ObjectKey)
		}

		if err := c.DeleteObjects(context.Background(), objectKeys); err != nil {
			log.Printf("failed to rollback partial upload prefix=%s objects=%d err=%v", remotePrefix, len(objectKeys), err)
			return
		}
		log.Printf("rolled back partial upload prefix=%s objects=%d", remotePrefix, len(objectKeys))
	}()

	err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return fmt.Errorf("build relative path: %w", err)
		}
		objectKey := strings.Trim(strings.ReplaceAll(filepath.ToSlash(filepath.Join(remotePrefix, rel)), "\\", "/"), "/")
		if err := c.UploadFile(ctx, objectKey, path, DetectContentType(path)); err != nil {
			return err
		}

		if err := c.VerifyObject(ctx, objectKey, info.Size()); err != nil {
			return err
		}

		uploaded = append(uploaded, UploadedObject{
			ObjectKey: objectKey,
			Size:      info.Size(),
		})
		return nil
	})
	if err != nil {
		rollbackRequired = true
		return nil, err
	}

	return uploaded, nil
}

func (c *R2Client) CopyPrefixVerified(ctx context.Context, sourcePrefix, destinationPrefix string) ([]UploadedObject, error) {
	sourcePrefix = strings.Trim(strings.TrimSpace(sourcePrefix), "/")
	destinationPrefix = strings.Trim(strings.TrimSpace(destinationPrefix), "/")
	if sourcePrefix == "" || destinationPrefix == "" {
		return nil, fmt.Errorf("sourcePrefix and destinationPrefix are required")
	}

	copied := make([]UploadedObject, 0)
	rollbackRequired := false
	defer func() {
		if !rollbackRequired || len(copied) == 0 {
			return
		}

		objectKeys := make([]string, 0, len(copied))
		for _, item := range copied {
			objectKeys = append(objectKeys, item.ObjectKey)
		}

		if err := c.DeleteObjects(context.Background(), objectKeys); err != nil {
			log.Printf("failed to rollback partial publish sourcePrefix=%s destinationPrefix=%s objects=%d err=%v", sourcePrefix, destinationPrefix, len(objectKeys), err)
			return
		}
		log.Printf("rolled back partial publish sourcePrefix=%s destinationPrefix=%s objects=%d", sourcePrefix, destinationPrefix, len(objectKeys))
	}()

	paginator := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(sourcePrefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			rollbackRequired = true
			return nil, fmt.Errorf("list objects for prefix %s: %w", sourcePrefix, err)
		}

		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}

			relativePath := strings.TrimPrefix(*object.Key, sourcePrefix)
			relativePath = strings.TrimPrefix(relativePath, "/")
			destinationKey := destinationPrefix
			if relativePath != "" {
				destinationKey = strings.Trim(filepath.ToSlash(filepath.Join(destinationPrefix, relativePath)), "/")
			}

			if err := c.CopyObject(ctx, *object.Key, destinationKey); err != nil {
				rollbackRequired = true
				return nil, err
			}

			if err := c.VerifyObject(ctx, destinationKey, aws.ToInt64(object.Size)); err != nil {
				rollbackRequired = true
				return nil, err
			}

			copied = append(copied, UploadedObject{
				ObjectKey: destinationKey,
				Size:      aws.ToInt64(object.Size),
			})
		}
	}

	return copied, nil
}

func (c *R2Client) VerifyObject(ctx context.Context, objectKey string, expectedSize int64) error {
	resp, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(strings.TrimLeft(objectKey, "/")),
	})
	if err != nil {
		return fmt.Errorf("head object %s: %w", objectKey, err)
	}

	actualSize := aws.ToInt64(resp.ContentLength)
	if actualSize != expectedSize {
		return fmt.Errorf(
			"object %s size mismatch after upload: expected %d bytes, got %d bytes",
			objectKey,
			expectedSize,
			actualSize,
		)
	}

	return nil
}

func (c *R2Client) CopyObject(ctx context.Context, sourceKey, destinationKey string) error {
	copySource := fmt.Sprintf("%s/%s", c.bucket, strings.TrimLeft(sourceKey, "/"))
	_, err := c.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(c.bucket),
		CopySource: aws.String(copySource),
		Key:        aws.String(strings.TrimLeft(destinationKey, "/")),
	})
	if err != nil {
		return fmt.Errorf("copy object %s to %s: %w", sourceKey, destinationKey, err)
	}
	return nil
}

func (c *R2Client) DeleteObject(ctx context.Context, objectKey string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(strings.TrimLeft(objectKey, "/")),
	})
	if err != nil {
		return fmt.Errorf("delete object %s: %w", objectKey, err)
	}
	return nil
}

func (c *R2Client) DeleteObjects(ctx context.Context, objectKeys []string) error {
	for _, objectKey := range objectKeys {
		if err := c.DeleteObject(ctx, objectKey); err != nil {
			return err
		}
	}
	return nil
}

func (c *R2Client) DeletePrefix(ctx context.Context, prefix string) error {
	prefix = strings.TrimLeft(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return nil
	}

	paginator := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})

	var objectKeys []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects for prefix %s: %w", prefix, err)
		}
		for _, object := range page.Contents {
			if object.Key != nil {
				objectKeys = append(objectKeys, *object.Key)
			}
		}
	}

	return c.DeleteObjects(ctx, objectKeys)
}

func (c *R2Client) DeletePrefixContentsExcept(ctx context.Context, prefix, keepPrefix string) error {
	prefix = strings.TrimLeft(strings.TrimSpace(prefix), "/")
	keepPrefix = strings.TrimLeft(strings.TrimSpace(keepPrefix), "/")
	if prefix == "" {
		return nil
	}

	if keepPrefix != "" && !strings.HasSuffix(keepPrefix, "/") {
		keepPrefix += "/"
	}

	paginator := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})

	objectKeys := make([]string, 0)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects for prefix %s: %w", prefix, err)
		}

		for _, object := range page.Contents {
			if object.Key == nil {
				continue
			}
			if keepPrefix != "" && strings.HasPrefix(*object.Key, keepPrefix) {
				continue
			}
			objectKeys = append(objectKeys, *object.Key)
		}
	}

	return c.DeleteObjects(ctx, objectKeys)
}

func (c *R2Client) BucketName() string {
	return c.bucket
}

func (c *R2Client) EndpointURL() string {
	return fmt.Sprintf("https://%s", strings.TrimPrefix(aws.ToString(c.client.Options().BaseEndpoint), "https://"))
}

func (c *R2Client) UploadDirLegacy(ctx context.Context, localDir, remotePrefix string) error {
	return filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return fmt.Errorf("build relative path: %w", err)
		}
		objectKey := strings.Trim(strings.ReplaceAll(filepath.ToSlash(filepath.Join(remotePrefix, rel)), "\\", "/"), "/")
		return c.UploadFile(ctx, objectKey, path, DetectContentType(path))
	})
}

func (c *R2Client) Download(ctx context.Context, objectKey string) ([]byte, string, error) {
	resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(strings.TrimLeft(objectKey, "/")),
	})
	if err != nil {
		return nil, "", fmt.Errorf("get object %s: %w", objectKey, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read object %s: %w", objectKey, err)
	}

	contentType := ""
	if resp.ContentType != nil {
		contentType = *resp.ContentType
	}

	return payload, contentType, nil
}
