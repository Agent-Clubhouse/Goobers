//go:build integration && cloudpg

package cismoke

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/jackc/pgx/v5"
)

func TestIntegrationCloudServices(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, requiredEnv(t, "GOOBERS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("SELECT 1 = %d, %v", one, err)
	}

	credential, err := azblob.NewSharedKeyCredential(
		requiredEnv(t, "GOOBERS_TEST_AZURITE_ACCOUNT"),
		requiredEnv(t, "GOOBERS_TEST_AZURITE_KEY"),
	)
	if err != nil {
		t.Fatal(err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential(
		requiredEnv(t, "GOOBERS_TEST_AZURITE_URL"),
		credential,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	container := "cloud-ci"
	_, _ = client.DeleteContainer(ctx, container, nil)
	if _, err := client.CreateContainer(ctx, container, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = client.DeleteContainer(context.Background(), container, nil) })
	want := []byte("azurite-ready")
	if _, err := client.UploadBuffer(ctx, container, "smoke.txt", want, nil); err != nil {
		t.Fatal(err)
	}
	response, err := client.DownloadStream(ctx, container, "smoke.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	got, err := io.ReadAll(response.Body)
	if err != nil || string(got) != string(want) {
		t.Fatalf("download = %q, %v", got, err)
	}
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}
