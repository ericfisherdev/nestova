package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"

	// feature/s3/manager.Uploader is flagged deprecated in favor of the
	// newer feature/s3/transfermanager — a DELIBERATE choice to stay on it
	// anyway: transfermanager is still pre-1.0 (v0.x, breaking-change-prone)
	// at the time this adapter was built, while manager is a stable, widely
	// used v1.x API with no functional gap for this adapter's needs
	// (streaming a single staged file, optionally multipart for a large
	// one). Revisit once transfermanager reaches 1.0.
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager" //nolint:staticcheck // SA1019: see above
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

// S3Params configures NewS3PhotoStore (NES-132). It mirrors
// config.S3Config field-for-field but is its own type: an adapter package
// depends on configuration only through the composition root (cmd/server)
// passing plain values in, never by importing internal/platform/config
// directly (DIP — adapters and platform/config are peer packages, neither
// depends on the other).
type S3Params struct {
	// Endpoint is the S3-compatible API's base URL; blank targets real AWS
	// S3's regional default endpoint. A custom endpoint (MinIO/Garage on the
	// LAN, or Cloudflare R2) is a first-class target, not an afterthought.
	Endpoint string
	// Region is required (AWS S3 needs a real one; most self-hosted
	// S3-compatible servers accept any non-empty value).
	Region string
	// Bucket is the single bucket every photo (both classes) is stored
	// under.
	Bucket string
	// AccessKeyID / SecretAccessKey are optional static credentials; when
	// both are blank, the AWS SDK's default credential chain supplies
	// credentials instead.
	AccessKeyID     string
	SecretAccessKey string
	// UsePathStyle forces path-style bucket addressing, required by MinIO
	// and most self-hosted S3-compatible servers.
	UsePathStyle bool
	// PresignTTL is URL's applied default when a caller passes a
	// non-positive ttl.
	PresignTTL time.Duration
	// MaxUploadBytes caps a single photo upload, mirroring
	// LocalPhotoStore's identical cap — the same operator-configured limit
	// applies regardless of backend.
	MaxUploadBytes int64
}

// S3PhotoStore is a domain.PhotoStore (and domain.ObjectLister) backed by an
// S3-compatible object store — AWS S3, or a self-hosted MinIO/Garage
// endpoint on the LAN, or Cloudflare R2 (NES-132). Photos use the identical
// class-namespaced, content-addressed key layout LocalPhotoStore uses (see
// buildStorageKey) — StorageRef IS the S3 object key verbatim, so a photo's
// ref means the same thing regardless of which backend stored it.
//
// Put stages every upload to a local temp file first (see validateAndStage),
// applying the EXACT same validation LocalPhotoStore.Put does, then uploads
// the validated file — a deliberate choice over pure byte-streaming straight
// to S3 while hashing/validating in flight: identical validation guarantees
// across backends (accept-list sniffing, size cap, full image decode) matter
// more for a family appliance than shaving the one local-disk round trip an
// upload already takes, and the decode-validation step needs the complete
// bytes anyway (a partially-streamed image cannot be decoded). "Never buffer
// the whole upload in memory" — the port's documented guarantee — is still
// honored: the temp file is how bytes are staged, never a []byte.
//
// Open buffers the complete object into memory, bounded by MaxUploadBytes
// (see Open's own doc for why: a GetObject response body is sequential-read
// only, but domain.PhotoReader needs ReadAt/Seek for EXIF extraction, and
// every genuinely-stored photo is already capped at MaxUploadBytes by Put —
// so this is a bounded, honest tradeoff, not unbounded memory growth, for
// photos that top out at 25 MiB in practice).
//
// Delete never errors on a missing key (S3 DeleteObject is idempotent by
// design), mirroring LocalPhotoStore.Delete's identical contract.
type S3PhotoStore struct {
	client  *s3.Client
	presign *s3.PresignClient
	// uploader: see the "feature/s3/manager" import's doc for why the
	// deprecated manager.Uploader is used deliberately, not by oversight.
	uploader       *manager.Uploader //nolint:staticcheck // SA1019: see the import's doc
	bucket         string
	maxUploadBytes int64
	presignTTL     time.Duration
	// requestSSE gates whether Put asks for SSE-S3 (AES256) — see Put's own
	// doc for why this is endpoint-conditional, not unconditional as
	// originally planned: verified against a real MinIO instance (no KMS
	// configured), MinIO does NOT silently ignore/no-op an SSE-S3 request as
	// assumed — it rejects the PutObject outright with 501 NotImplemented
	// ("Server side encryption specified but KMS is not configured"). SSE-S3
	// is therefore requested only against real AWS S3 (no custom Endpoint);
	// a custom endpoint (MinIO/Garage/R2) never gets the header at all,
	// which is also simply correct for R2/Garage, whose own encryption
	// models don't take an S3-protocol SSE header either.
	requestSSE bool
}

var (
	_ domain.PhotoStore      = (*S3PhotoStore)(nil)
	_ domain.ObjectLister    = (*S3PhotoStore)(nil)
	_ domain.ObjectExister   = (*S3PhotoStore)(nil)
	_ domain.RawObjectWriter = (*S3PhotoStore)(nil)
)

// NewS3PhotoStore builds an S3PhotoStore against params and verifies the
// configured bucket is reachable (HeadBucket) before returning, so a
// misconfigured endpoint, bucket, or credentials fails the boot loudly here
// rather than surfacing as an opaque error on the household's first photo
// upload.
func NewS3PhotoStore(ctx context.Context, params S3Params) (*S3PhotoStore, error) {
	switch {
	case strings.TrimSpace(params.Bucket) == "":
		return nil, errors.New("media/adapter: S3 photo store bucket must not be blank")
	case strings.TrimSpace(params.Region) == "":
		return nil, errors.New("media/adapter: S3 photo store region must not be blank")
	case params.MaxUploadBytes <= 0:
		return nil, fmt.Errorf("media/adapter: max upload bytes must be positive, got %d", params.MaxUploadBytes)
	case params.PresignTTL <= 0:
		return nil, fmt.Errorf("media/adapter: presign ttl must be positive, got %v", params.PresignTTL)
	}

	optFns := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(params.Region)}
	if params.AccessKeyID != "" && params.SecretAccessKey != "" {
		optFns = append(optFns, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(params.AccessKeyID, params.SecretAccessKey, ""),
		))
	}
	// Otherwise the SDK's default credential chain (environment, shared
	// config/credentials file, EC2/ECS instance role, etc.) applies
	// unchanged — see S3Config.AccessKeyID's doc for why this is supported
	// both ways.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("media/adapter: load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if params.Endpoint != "" {
			o.BaseEndpoint = aws.String(params.Endpoint)
		}
		o.UsePathStyle = params.UsePathStyle
	})

	store := &S3PhotoStore{
		client:         client,
		presign:        s3.NewPresignClient(client),
		uploader:       manager.NewUploader(client), //nolint:staticcheck // SA1019: see the import's doc
		bucket:         params.Bucket,
		maxUploadBytes: params.MaxUploadBytes,
		presignTTL:     params.PresignTTL,
		// See the requestSSE field doc: only real AWS S3 (no custom
		// Endpoint) gets the SSE-S3 header.
		requestSSE: params.Endpoint == "",
	}

	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(params.Bucket)}); err != nil {
		return nil, fmt.Errorf("media/adapter: bucket %q is not reachable via endpoint %q: %w", params.Bucket, params.Endpoint, err)
	}
	return store, nil
}

// Put validates and stages r exactly as LocalPhotoStore.Put does (see the
// type doc), then streams the staged file to S3 via the manager.Uploader
// (multipart for a large file, single PUT otherwise), requesting SSE-S3
// (AES256) only against real AWS S3 (see the requestSSE field doc for why:
// verified against a real MinIO instance without KMS configured, MinIO
// rejects an SSE-S3 PutObject outright with 501 NotImplemented rather than
// silently ignoring it, so requesting it unconditionally would break every
// MinIO/Garage deployment this ticket's custom-endpoint support exists for).
func (s *S3PhotoStore) Put(ctx context.Context, householdID household.HouseholdID, class domain.PhotoClass, r io.Reader) (domain.PutResult, error) {
	if !class.Valid() {
		return domain.PutResult{}, fmt.Errorf("media/adapter: unknown photo class %d", class)
	}
	staged, err := validateAndStage(os.TempDir(), s.maxUploadBytes, r)
	if err != nil {
		return domain.PutResult{}, err
	}
	defer removeStaged(staged.Path)

	key, err := buildStorageKey(householdID, class, staged.ContentHash, acceptedTypes[staged.ContentType])
	if err != nil {
		return domain.PutResult{}, err
	}

	f, err := os.Open(staged.Path)
	if err != nil {
		return domain.PutResult{}, fmt.Errorf("media/adapter: reopen staged upload: %w", err)
	}
	defer func() { _ = f.Close() }()

	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        f,
		ContentType: aws.String(staged.ContentType),
		// Baked into the object's own metadata at upload time — see
		// photoBytesCacheControl's doc for why this must not fall back to
		// whatever permissive default the object store would otherwise
		// apply, and URL's ResponseCacheControl below for the belt-and-
		// suspenders reassertion on each individual presigned GET.
		CacheControl: aws.String(photoBytesCacheControl),
	}
	if s.requestSSE {
		input.ServerSideEncryption = types.ServerSideEncryptionAes256
	}
	if _, err := s.uploader.Upload(ctx, input); err != nil { //nolint:staticcheck // SA1019: see the import's doc
		return domain.PutResult{}, fmt.Errorf("media/adapter: upload photo to s3: %w", err)
	}

	return domain.PutResult{
		Ref: domain.StorageRef(key), ContentHash: staged.ContentHash,
		SizeBytes: staged.SizeBytes, ContentType: staged.ContentType,
	}, nil
}

// Open fetches ref and buffers it fully into memory (see the type doc for
// why), returning domain.ErrPhotoNotFound when the key does not exist.
func (s *S3PhotoStore) Open(ctx context.Context, ref domain.StorageRef) (domain.PhotoReader, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(ref.String()),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, fmt.Errorf("%w: %s", domain.ErrPhotoNotFound, ref)
		}
		return nil, fmt.Errorf("media/adapter: get photo from s3: %w", err)
	}
	defer func() { _ = out.Body.Close() }()

	// A stored object can never legitimately exceed maxUploadBytes (Put
	// enforces that at upload time); the +1 limit is defense-in-depth
	// against an object written some other way, never against a normally-
	// stored photo.
	data, err := io.ReadAll(io.LimitReader(out.Body, s.maxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("media/adapter: read photo from s3: %w", err)
	}
	if int64(len(data)) > s.maxUploadBytes {
		return nil, fmt.Errorf("media/adapter: object %s exceeds the %d-byte limit", ref, s.maxUploadBytes)
	}
	return s3PhotoReader{bytes.NewReader(data)}, nil
}

// s3PhotoReader adapts a fully-buffered *bytes.Reader (already satisfying
// Read/ReadAt/Seek) into a domain.PhotoReader with a no-op Close — see
// S3PhotoStore.Open's doc for why buffering the whole object is the
// deliberate, bounded tradeoff here.
type s3PhotoReader struct {
	*bytes.Reader
}

func (s3PhotoReader) Close() error { return nil }

// Delete removes ref's object; a missing key is not an error (S3
// DeleteObject is idempotent), mirroring LocalPhotoStore.Delete.
func (s *S3PhotoStore) Delete(ctx context.Context, ref domain.StorageRef) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(ref.String()),
	}); err != nil {
		return fmt.Errorf("media/adapter: delete photo from s3: %w", err)
	}
	return nil
}

// URL confirms ref exists (HeadObject — presigning alone never verifies
// existence, and the port's contract requires ErrPhotoNotFound for an
// unknown ref, mirroring LocalPhotoStore.URL's os.Stat check) and returns a
// presigned GET URL valid for ttl, or s.presignTTL when ttl is non-positive.
func (s *S3PhotoStore) URL(ctx context.Context, ref domain.StorageRef, ttl time.Duration) (string, error) {
	if _, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(ref.String())}); err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return "", fmt.Errorf("%w: %s", domain.ErrPhotoNotFound, ref)
		}
		return "", fmt.Errorf("media/adapter: check photo exists in s3: %w", err)
	}
	if ttl <= 0 {
		ttl = s.presignTTL
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(ref.String()),
		// ResponseCacheControl overrides the response header this specific
		// presigned GET returns, reasserting photoBytesCacheControl
		// regardless of what CacheControl ended up stored on the object
		// (Put already sets it too — see that call's doc — this is belt
		// and suspenders, not a substitute).
		ResponseCacheControl: aws.String(photoBytesCacheControl),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("media/adapter: presign photo url: %w", err)
	}
	return req.URL, nil
}

// SupportsDirectURL always reports true: S3PhotoStore's URL returns a real,
// browser-navigable presigned GET a caller may safely redirect a client to.
func (s *S3PhotoStore) SupportsDirectURL() bool { return true }

// ListObjects returns every object stored under class's namespace, across
// every household (domain.ObjectLister; NES-132/133's storage reaper). The
// key layout (buildStorageKey: households/<household>/<class-prefix>/...)
// puts <household> BEFORE <class-prefix>, so no single S3 ListObjectsV2
// Prefix can select "every household's objects of one class" directly —
// this lists the whole households/ tree and filters by class client-side
// (see keyBelongsToClass). For this app's expected scale (a single family
// household's photo library, not a multi-tenant SaaS bucket with millions
// of objects), a full-tree scan per reaper pass is an acceptable cost; a
// future NES-133 optimization could restructure the key layout (class
// before household) if that ever stops being true.
func (s *S3PhotoStore) ListObjects(ctx context.Context, class domain.PhotoClass) ([]domain.ObjectInfo, error) {
	classPrefix, err := classKeyPrefix(class)
	if err != nil {
		return nil, err
	}
	objects := make([]domain.ObjectInfo, 0)
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String("households/"),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("media/adapter: list s3 objects: %w", err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if !keyBelongsToClass(key, classPrefix) {
				continue
			}
			objects = append(objects, domain.ObjectInfo{
				Key:          domain.StorageRef(key),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
	}
	return objects, nil
}

// ObjectExists reports whether ref is already stored (an S3 HeadObject,
// verbatim) without downloading it — NES-133's storage migrator's
// idempotency check (domain.ObjectExister) before uploading a
// content-addressed object a different row's migration may have already
// written at the same key (see PutAt's doc).
func (s *S3PhotoStore) ObjectExists(ctx context.Context, ref domain.StorageRef) (bool, error) {
	if _, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(ref.String())}); err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("media/adapter: check object exists in s3: %w", err)
	}
	return true, nil
}

// PutAt uploads r's bytes to ref verbatim (domain.RawObjectWriter) — no
// content sniffing, decode-validation, or hashing, since the caller has
// already done all of that once (see RawObjectWriter's doc for why this,
// not Put, is what NES-133's storage migrator calls). Mirrors Put's own
// SSE-S3/Cache-Control handling exactly (see Put's doc and the requestSSE
// field doc for why SSE-S3 is endpoint-conditional), so an object the
// migrator writes is indistinguishable from one a normal upload would have
// produced at the same key.
func (s *S3PhotoStore) PutAt(ctx context.Context, ref domain.StorageRef, contentType string, r io.Reader) error {
	input := &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(ref.String()),
		Body:         r,
		ContentType:  aws.String(contentType),
		CacheControl: aws.String(photoBytesCacheControl),
	}
	if s.requestSSE {
		input.ServerSideEncryption = types.ServerSideEncryptionAes256
	}
	if _, err := s.uploader.Upload(ctx, input); err != nil { //nolint:staticcheck // SA1019: see the import's doc
		return fmt.Errorf("media/adapter: put object to s3: %w", err)
	}
	return nil
}

// keyBelongsToClass reports whether key's class-prefix path segment
// ("households/<household>/<class-prefix>/...") equals classPrefix — see
// ListObjects' doc for why this client-side filter exists.
func keyBelongsToClass(key, classPrefix string) bool {
	parts := strings.SplitN(key, "/", 4)
	return len(parts) >= 3 && parts[0] == "households" && parts[2] == classPrefix
}
