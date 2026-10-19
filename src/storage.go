package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ---------------- storage: S3 (minio-go v7) --------------------

// s3Client is the cached minio client. It is rebuilt when the endpoint,
// credentials, or region change so fixing config (or a failed first build)
// takes effect without a restart.
var (
	s3ClientMtx sync.Mutex
	s3Client    *minio.Client
	s3ClientKey string
)

// getS3Client builds (or reuses) the minio client for the current config,
// using path-style addressing for S3-compatible services
// (MinIO, Cloudflare R2, Garage, Scaleway, etc).
func getS3Client(app *App) (*minio.Client, error) {
	var sc = app.cfg.Storage.S3
	endpoint := strings.TrimSpace(sc.Endpoint)
	secure := strings.HasPrefix(strings.ToLower(endpoint), "https://")
	// Strip the scheme case-insensitively: a "HTTPS://host" endpoint used to
	// keep its scheme (TrimPrefix is case-sensitive) and minio.New then failed
	// on the malformed endpoint.
	if i := strings.Index(endpoint, "://"); i >= 0 {
		endpoint = endpoint[i+3:]
	}
	endpoint = strings.TrimRight(endpoint, "/")
	region := sc.Region
	if region == "" {
		region = "us-east-1"
	}
	if endpoint == "" {
		return nil, fmt.Errorf("s3 not configured (endpoint empty)")
	}
	var tlsStr = "plain"
	if secure {
		tlsStr = "tls"
	}
	var key = strings.ToLower(endpoint) + "|" + tlsStr + "|" + region + "|" + sc.AccessKey + "|" + sc.SecretKey
	s3ClientMtx.Lock()
	defer s3ClientMtx.Unlock()
	if s3Client != nil && s3ClientKey == key {
		return s3Client, nil
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(sc.AccessKey, sc.SecretKey, ""),
		Secure: secure,
		Region: region,
	})
	if err != nil {
		return nil, err
	}
	s3Client = client
	s3ClientKey = key
	return s3Client, nil
}

// s3PutFile streams a local file to S3 without buffering it in RAM: clips can
// be hundreds of MB and must never be pulled into memory as a whole.
func s3PutFile(app *App, key string, path string, contentType string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, serr := f.Stat()
	if serr != nil {
		return serr
	}
	if st.Size() == 0 {
		return fmt.Errorf("s3 upload skipped (empty object)")
	}
	return s3PutObject(app, key, f, st.Size(), contentType)
}

// s3PutObject is the shared upload path for the blob and streaming cases.
func s3PutObject(app *App, key string, r io.Reader, size int64, contentType string) error {
	var sc = app.cfg.Storage.S3
	if !s3UploadEnabled() {
		return fmt.Errorf("s3 upload is disabled")
	}
	if sc.Bucket == "" || sc.Endpoint == "" {
		return fmt.Errorf("s3 not configured (endpoint or bucket empty)")
	}
	if size <= 0 {
		return fmt.Errorf("s3 upload skipped (empty object)")
	}
	client, err := getS3Client(app)
	if err != nil {
		return err
	}
	var storageClass = "ONEZONE_IA"
	if sc.StorageClass != "" {
		storageClass = sc.StorageClass
	}
	var fullKey = key
	if sc.Prefix != "" {
		fullKey = strings.Trim(sc.Prefix, "/") + "/" + strings.TrimLeft(key, "/")
	}
	// Bounded: a hung S3 must not pin the assembleClip/retry worker forever.
	putCtx, putCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer putCancel()
	_, err = client.PutObject(putCtx, sc.Bucket, fullKey, r, size, minio.PutObjectOptions{
		ContentType:  contentType,
		StorageClass: storageClass,
	})
	if err != nil {
		return err
	}
	logIt(LINFO, "s3 object uploaded", map[string]string{"key": fullKey, "bucket": sc.Bucket, "bytes": strconv.FormatInt(size, 10)})
	return nil
}

// s3Backoff returns the retry delay for a 1-based attempt number:
// 5s, 10s, 20s, 40s, capped at 60s.
func s3Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	var d = 5 * time.Second
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= 60*time.Second {
			return 60 * time.Second
		}
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// s3PutJob is one file upload within a multi-file detection upload.
type s3PutJob struct {
	key         string
	path        string
	contentType string
}

// s3PutFiles uploads jobs concurrently with 4 workers. Returns true only when
// every job succeeded.
func s3PutFiles(app *App, jobs []s3PutJob) bool {
	if len(jobs) == 0 {
		return true
	}
	var sem = make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok = true
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := s3PutFile(app, j.key, j.path, j.contentType); err != nil {
				logIt(LWARN, "s3 upload failed", map[string]string{"key": j.key, "path": j.path, "error": err.Error()})
				mu.Lock()
				ok = false
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return ok
}

// s3DeleteObject removes an object key (before the prefix) from S3 when storage
// is enabled. The configured prefix is applied identically to s3PutObject.
func s3DeleteObject(app *App, key string) error {
	var sc = app.cfg.Storage.S3
	// Deletes ignore the runtime upload toggle (it gates uploads only) so
	// disabling S3 then deleting a recording cannot orphan S3 objects.
	if sc.Endpoint == "" || sc.Bucket == "" {
		return nil
	}
	client, err := getS3Client(app)
	if err != nil {
		return err
	}
	var fullKey = key
	if sc.Prefix != "" {
		fullKey = strings.Trim(sc.Prefix, "/") + "/" + strings.TrimLeft(key, "/")
	}
	delCtx, delCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer delCancel()
	err = client.RemoveObject(delCtx, sc.Bucket, fullKey, minio.RemoveObjectOptions{})
	if err != nil {
		return err
	}
	logIt(LINFO, "s3 object deleted", map[string]string{"key": fullKey, "bucket": sc.Bucket})
	return nil
}

// s3KeyForPath derives the S3 object key (without prefix) for a local media
// file, mirroring how uploads name objects (detections/<date>/<cam>/<file>
// or manual/<cam>/<file>).
func s3KeyForPath(path string) string {
	for _, marker := range []string{"/detections/", "/manual/"} {
		var parts = strings.Split(path, marker)
		if len(parts) == 2 {
			return strings.Trim(marker, "/") + "/" + parts[1]
		}
	}
	return strings.TrimLeft(path, "/")
}

// deleteS3Files removes the given local media files from S3. Failures are
// logged (not fatal) so silent S3 leftovers become visible in the log.
func deleteS3Files(app *App, paths []string) {
	// Same as s3DeleteObject: deletes must run even when uploads are toggled off.
	if app.cfg.Storage.S3.Endpoint == "" || app.cfg.Storage.S3.Bucket == "" {
		return
	}
	var failed int
	for _, p := range paths {
		if p != "" {
			if err := s3DeleteObject(app, s3KeyForPath(p)); err != nil {
				failed++
				logIt(LWARN, "s3 delete failed", map[string]string{"key": s3KeyForPath(p), "error": err.Error()})
			}
		}
	}
	if failed > 0 {
		logIt(LWARN, "s3 deletes incomplete", map[string]string{"failed": strconv.Itoa(failed)})
	}
}
