// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022-present Datadog, Inc.

package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"

	pb "github.com/DataDog/datadog-agent/pkg/proto/pbgo/trace"
	"github.com/DataDog/datadog-agent/pkg/trace/api/internal/header"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/testutil"

	"github.com/DataDog/datadog-go/v5/statsd"
	"github.com/stretchr/testify/assert"
)

func TestConnContext(t *testing.T) {
	sockPath := "/tmp/test-trace.sock"
	payload := msgpTraces(t, pb.Traces{testutil.RandomTrace(10, 20)})
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	fi, err := os.Stat(sockPath)
	if err == nil {
		// already exists
		if fi.Mode()&os.ModeSocket == 0 {
			t.Fatalf("cannot reuse %q; not a unix socket", sockPath)
		}
		if err := os.Remove(sockPath); err != nil {
			t.Fatalf("unable to remove stale socket: %v", err)
		}
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("error listening on unix socket %s: %v", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o722); err != nil {
		t.Fatalf("error setting socket permissions: %v", err)
	}
	ln = NewMeasuredListener(ln, "uds_connections", 10, &statsd.NoOpClient{})
	defer ln.Close()

	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ucred, ok := r.Context().Value(ucredKey{}).(*syscall.Ucred)
			if !ok || ucred == nil {
				t.Fatalf("Expected a unix credential but found nothing.")
			}
			io.WriteString(w, "OK")
		}),
		ConnContext: connContext,
	}
	go s.Serve(ln)

	resp, err := client.Post("http://localhost:8126/v0.4/traces", "application/msgpack", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected http.StatusOK, got response: %#v", resp)
	}
}

func TestGetContainerID(t *testing.T) {
	const containerID = "abcdef"
	const containerPID = 1234
	const containerInode = "4242"

	// LocalData header prefixes
	const (
		legacyContainerIDPrefix = "cid-"
		containerIDPrefix       = "ci-"
		inodePrefix             = "in-"
	)

	provider := &cgroupIDProvider{
		procRoot:   "",
		controller: "",
	}

	t.Run("ContainerID header", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.ContainerID, containerID)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("ContainerID header with PID", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), ucredKey{}, &syscall.Ucred{Pid: containerPID})
		req, err := http.NewRequestWithContext(ctx, "GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.ContainerID, containerID)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("ContainerID header with LocalData header", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.ContainerID, containerID)
		req.Header.Add(header.LocalData, containerIDPrefix+containerID+","+inodePrefix+containerInode)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("ContainerID header with invalid LocalData header", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.ContainerID, containerID)
		req.Header.Add(header.LocalData, containerIDPrefix+containerID+","+inodePrefix+containerInode)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("Invalid ContainerID header with LocalData header", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.ContainerID, "wrong_container_id")
		req.Header.Add(header.LocalData, containerIDPrefix+containerID)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("LocalData header with old container ID format", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.LocalData, legacyContainerIDPrefix+containerID)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("LocalData header with new container ID format", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.LocalData, containerIDPrefix+containerID)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	validLocalDataLists := []string{
		containerIDPrefix + containerID + "," + containerInode + containerInode,
		inodePrefix + containerInode + "," + containerIDPrefix + containerID,
		containerIDPrefix + containerID + "," + inodePrefix,
		inodePrefix + "," + containerIDPrefix + containerID,
		containerIDPrefix + containerID + ",",
		"," + containerIDPrefix + containerID,
		"," + containerIDPrefix + containerID + ",",
	}
	for index, validLocalDataList := range validLocalDataLists {
		t.Run(fmt.Sprintf("LocalData header as a list (%d/%d)", index, len(validLocalDataLists)), func(t *testing.T) {
			req, err := http.NewRequest("GET", "http://example.com", nil)
			if !assert.NoError(t, err) {
				t.Fail()
			}
			req.Header.Add(header.LocalData, validLocalDataList)
			assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
		})
	}

	// Test invalid LocalData headers
	provider.containerIDFromOriginInfo = config.NoopContainerIDFromOriginInfoFunc
	invalidLocalDataLists := []string{
		"",
		",",
		containerIDPrefix + ",",
		inodePrefix + ",",
		containerIDPrefix + "," + inodePrefix,
		"," + containerIDPrefix + "," + inodePrefix + ",",
	}
	for index, invalidLocalDataList := range invalidLocalDataLists {
		t.Run(fmt.Sprintf("LocalData header as an invalid list (%d/%d)", index, len(invalidLocalDataLists)), func(t *testing.T) {
			req, err := http.NewRequest("GET", "http://example.com", nil)
			if !assert.NoError(t, err) {
				t.Fail()
			}
			req.Header.Add(header.LocalData, invalidLocalDataList)
			assert.Equal(t, "", provider.GetContainerID(req.Context(), req.Header))
		})
	}
	t.Run("LocalData header with PID", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), ucredKey{}, &syscall.Ucred{Pid: containerPID})
		req, err := http.NewRequestWithContext(ctx, "GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		req.Header.Add(header.LocalData, legacyContainerIDPrefix+containerID)
		assert.Equal(t, containerID, provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("No header with an invalid PID", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), ucredKey{}, &syscall.Ucred{Pid: 2345})
		req, err := http.NewRequestWithContext(ctx, "GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		assert.Equal(t, "", provider.GetContainerID(req.Context(), req.Header))
	})

	t.Run("No header with no PID", func(t *testing.T) {
		req, err := http.NewRequest("GET", "http://example.com", nil)
		if !assert.NoError(t, err) {
			t.Fail()
		}
		assert.Equal(t, "", provider.GetContainerID(req.Context(), req.Header))
	})
}

func BenchmarkUDSCred(b *testing.B) {
	sockPath := "/tmp/test-trace.sock"
	client := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	fi, err := os.Stat(sockPath)
	if err == nil {
		// already exists
		if fi.Mode()&os.ModeSocket == 0 {
			b.Fatalf("cannot reuse %q; not a unix socket", sockPath)
		}
		if err := os.Remove(sockPath); err != nil {
			b.Fatalf("unable to remove stale socket: %v", err)
		}
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		b.Fatalf("error listening on unix socket %s: %v", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0o722); err != nil {
		b.Fatalf("error setting socket permissions: %v", err)
	}
	ln = NewMeasuredListener(ln, "uds_connections", 10, &statsd.NoOpClient{})
	defer ln.Close()

	recvbuf := make([]byte, 1024*1024*10) // 10MiB
	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ucred, ok := r.Context().Value(ucredKey{}).(*syscall.Ucred)
			if !ok || ucred == nil {
				b.Fatalf("Expected a unix credential but found nothing.")
			}
			// actually read the body, and respond afterwards, to force benchmarking of
			// io over the socket.
			io.ReadFull(r.Body, recvbuf)
			io.WriteString(w, "OK")
		}),
		ConnContext: connContext,
	}
	go s.Serve(ln)

	buf := make([]byte, 1024*1024*10) // 10MiB
	for i := 0; i < b.N; i++ {
		resp, err := client.Post("http://localhost:8126/v0.4/traces", "application/msgpack", bytes.NewReader(buf))
		if err != nil {
			b.Fatal(err)
		}
		// We don't read the response here to force a new connection for each request.
		//io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			b.Fatalf("expected http.StatusOK, got response: %#v", resp)
		}
	}
}
