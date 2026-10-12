package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func testNginxInclude(t *testing.T, root, include string, manifest, blob v1.Descriptor) {
	t.Helper()
	binary := os.Getenv("NGINX_TEST_BINARY")
	if binary == "" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	prefix := t.TempDir()
	host := filepath.Join(prefix, "host.conf")
	config := fmt.Sprintf(`error_log stderr;
pid "%[1]s/nginx.pid";
events {}
http {
    access_log off;
    client_body_temp_path "%[1]s/body";
    proxy_temp_path "%[1]s/proxy";
    fastcgi_temp_path "%[1]s/fastcgi";
    uwsgi_temp_path "%[1]s/uwsgi";
    scgi_temp_path "%[1]s/scgi";
    server {
        listen %[2]s;
        types { application/problem+json json; }
        gzip on;
        gzip_min_length 0;
        gzip_types *;
        include "%[3]s";
        location / { return 200 "application"; }
        location ~ ^/v2/ { return 418; }
    }
}
`, prefix, address, include)
	if err := os.WriteFile(host, []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(binary, "-t", "-p", prefix, "-c", host).CombinedOutput(); err != nil {
		t.Fatalf("nginx syntax: %v\n%s", err, output)
	}
	var output bytes.Buffer
	cmd := exec.Command(binary, "-p", prefix, "-c", host, "-g", "daemon off; worker_shutdown_timeout 1s;")
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: time.Second}
	t.Cleanup(func() {
		client.CloseIdleConnections()
		cmd.Process.Signal(syscall.SIGQUIT)
		if err := cmd.Wait(); err != nil {
			t.Errorf("nginx failed: %v\n%s", err, output.String())
		}
	})
	base := "http://" + address
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get(base + "/v2/")
		if err == nil {
			response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nginx startup: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	manifestPath := "/v2/test/image/manifests/v1.json"
	blobPath := "/v2/test/image/blobs/" + blob.Digest.String()
	for _, check := range []struct {
		method, path, contentType, digest, byteRange string
		status                                       int
	}{
		{"GET", "/v2/", "application/json", "", "", 200},
		{"GET", manifestPath, string(manifest.MediaType), manifest.Digest.String(), "", 200},
		{"HEAD", manifestPath, string(manifest.MediaType), manifest.Digest.String(), "", 200},
		{"HEAD", blobPath, "application/octet-stream", blob.Digest.String(), "", 200},
		{"GET", blobPath, "application/octet-stream", blob.Digest.String(), "", 200},
		{"GET", blobPath, "application/octet-stream", blob.Digest.String(), "bytes=0-2", 206},
		{"POST", "/v2/", "", "", "", 405},
		{"POST", manifestPath, "", "", "", 405},
		{"POST", blobPath, "", "", "", 405},
		{"GET", "/v2/test/image/manifests/.metadata.json", "", "", "", 404},
		{"POST", "/application", "", "", "", 200},
	} {
		request, err := http.NewRequest(check.method, base+check.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if check.path != "/application" {
			request.Header.Set("Accept-Encoding", "gzip")
		}
		if check.byteRange != "" {
			request.Header.Set("Range", check.byteRange)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != check.status {
			t.Fatalf("%s %s: status %d, %v", check.method, check.path, response.StatusCode, err)
		}
		if check.contentType != "" && response.Header.Get("Content-Type") != check.contentType {
			t.Errorf("%s %s: wrong Content-Type", check.method, check.path)
		}
		if check.digest != "" && response.Header.Get("Docker-Content-Digest") != check.digest {
			t.Errorf("%s %s: wrong digest header", check.method, check.path)
		}
		if check.method == "GET" && check.path == manifestPath {
			expected, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifestPath)))
			if err != nil || !bytes.Equal(body, expected) || response.Header.Get("Content-Encoding") != "" {
				t.Errorf("manifest was altered by the host configuration: %v", err)
			}
		}
		if check.byteRange != "" && len(body) != 3 {
			t.Errorf("Range returned %d bytes", len(body))
		}
		if check.method == "GET" && check.path == blobPath && check.byteRange == "" {
			if err := copyContent(io.Discard, bytes.NewReader(body), blob); err != nil {
				t.Errorf("blob was altered by the host configuration: %v", err)
			}
		}
		if check.path == "/application" && string(body) != "application" {
			t.Error("fragment affected host application")
		}
	}
}
