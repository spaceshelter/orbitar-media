// Package s3store keeps the long-term copy of every upload in S3 (key = the bare hash), which the
// local data directory is a cache of.
package s3store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Config mirrors PictShare's S3_* settings.
type Config struct {
	Bucket, Region, AccessKey, SecretKey, Endpoint string
}

// Client talks to one bucket.
type Client struct {
	c      *s3.Client
	bucket string
}

// New returns nil when no bucket is configured (S3 disabled).
func New(cfg Config) *Client {
	if cfg.Bucket == "" {
		return nil
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle: true,
		// plain PUTs: S3-compatible stores (and mocks) don't all speak the newer checksum headers
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	return &Client{c: s3.New(opts), bucket: cfg.Bucket}
}

// Exists reports whether key is in the bucket.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &key})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("not found in S3")

// Get downloads key to dst (at most maxBytes, 0 = unlimited).
func (c *Client) Get(ctx context.Context, key, dst string, maxBytes int64) error {
	out, err := c.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	defer out.Body.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	var r io.Reader = out.Body
	if maxBytes > 0 {
		r = io.LimitReader(out.Body, maxBytes+1)
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && maxBytes > 0 && n > maxBytes {
		err = fmt.Errorf("%s is larger than %d bytes", key, maxBytes)
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// Put uploads src as key.
func (c *Client) Put(ctx context.Context, key, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = c.c.PutObject(ctx, &s3.PutObjectInput{Bucket: &c.bucket, Key: &key, Body: f, ContentLength: aws.Int64(st.Size())})
	return err
}

// Delete removes key.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.bucket, Key: &key})
	return err
}

func isNotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	var re interface{ HTTPStatusCode() int }
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound
}
