package config

import (
	"strings"
	"testing"
)

func TestStorageEnvironment(t *testing.T) {
	setBaseline(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.StorageMode != "" || c.S3Region != "us-east-1" || c.StorageMaxObjectBytes != 25<<20 {
		t.Fatalf("defaults: %q %q %d", c.StorageMode, c.S3Region, c.StorageMaxObjectBytes)
	}

	t.Setenv("JANUS_STORAGE_MODE", "s3")
	t.Setenv("JANUS_S3_ENDPOINT", "http://seaweed:8333")
	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "JANUS_S3_BUCKET") || !strings.Contains(err.Error(), "JANUS_S3_SECRET_ACCESS_KEY") {
		t.Fatalf("half-configured s3 must name the missing variables: %v", err)
	}
	t.Setenv("JANUS_S3_BUCKET", "janus")
	t.Setenv("JANUS_S3_ACCESS_KEY_ID", "ak")
	t.Setenv("JANUS_S3_SECRET_ACCESS_KEY", "sk-value")
	t.Setenv("JANUS_S3_PATH_STYLE", "true")
	t.Setenv("JANUS_STORAGE_MAX_OBJECT_BYTES", "1048576")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.S3PathStyle || c.StorageMaxObjectBytes != 1<<20 || c.S3SecretAccessKey != "sk-value" {
		t.Fatalf("s3 env not applied: %+v", c.S3PathStyle)
	}
	t.Setenv("JANUS_S3_CA_FILE", "/nonexistent/ca.pem")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "JANUS_S3_CA_FILE") {
		t.Fatalf("bad CA file: %v", err)
	}
	t.Setenv("JANUS_S3_CA_FILE", "")
	t.Setenv("JANUS_STORAGE_MODE", "minio")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "JANUS_STORAGE_MODE") {
		t.Fatalf("bad mode: %v", err)
	}
	t.Setenv("JANUS_STORAGE_MODE", "database")
	t.Setenv("JANUS_STORAGE_MAX_OBJECT_BYTES", "-1")
	if _, err = Load(); err == nil {
		t.Fatal("negative max accepted")
	}
}
