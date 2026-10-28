package adapter

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// newPresignOnlyStore builds an S3PhotoStore that can presign but never talks
// to a server, which is all DirectURLOrigin needs: presigning is local.
func newPresignOnlyStore(endpoint string, pathStyle bool) *S3PhotoStore {
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("key", "secret", ""),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: pathStyle,
	})
	return &S3PhotoStore{presign: s3.NewPresignClient(client), bucket: "family-photos"}
}

func TestS3PhotoStoreDirectURLOrigin(t *testing.T) {
	cases := []struct {
		name      string
		endpoint  string
		pathStyle bool
		want      string
	}{
		{"path-style keeps the bucket out of the host", "http://minio.lan:9000", true, "http://minio.lan:9000"},
		{"virtual-hosted puts the bucket in the host", "https://s3.example.com", false, "https://family-photos.s3.example.com"},
		{"https endpoint with a port", "https://garage.lan:3900", true, "https://garage.lan:3900"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newPresignOnlyStore(tc.endpoint, tc.pathStyle).DirectURLOrigin(context.Background())
			if err != nil {
				t.Fatalf("DirectURLOrigin: %v", err)
			}
			if got != tc.want {
				t.Errorf("origin = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOriginOfRejectsURLsWithoutAnOrigin(t *testing.T) {
	for _, raw := range []string{"", "/photos/1/raw", "not a url", "://missing-scheme"} {
		if got, err := originOf(raw); err == nil {
			t.Errorf("originOf(%q) = %q, want an error", raw, got)
		}
	}
}
