package bandwidth

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestLimiterAllowAndRefill(t *testing.T) {
	limiter := NewLimiter(50)
	if !limiter.Allow(50) {
		t.Fatal("expected initial allowance to succeed")
	}
	if limiter.Allow(1) {
		t.Fatal("expected immediate extra allowance to fail")
	}

	time.Sleep(30 * time.Millisecond)
	if !limiter.Allow(1) {
		t.Fatal("expected bucket refill to allow a small read")
	}
}

func TestLimiterWaitBlocksUntilTokensAvailable(t *testing.T) {
	limiter := NewLimiter(50)
	if !limiter.Allow(50) {
		t.Fatal("expected initial allowance")
	}

	start := time.Now()
	limiter.Wait(10)
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("Wait() elapsed = %v, want at least 150ms", elapsed)
	}
}

func TestUnlimitedLimiterAndLimitedReader(t *testing.T) {
	limiter := NewLimiter(0)
	if !limiter.Allow(1 << 20) {
		t.Fatal("unlimited limiter should always allow")
	}

	reader := NewLimitedReader(bytes.NewBufferString("hello"), limiter)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", data)
	}
}
