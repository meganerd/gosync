package progress

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProgressUpdateCallbackAndString(t *testing.T) {
	p := NewProgress(1000, 4)
	p.startTime = time.Now().Add(-2 * time.Second)

	var callbackInfo ProgressInfo
	p.SetCallback(func(info ProgressInfo) {
		callbackInfo = info
	})

	p.Update(250, true)
	p.Update(100, false)

	info := p.GetInfo()
	if info.TransferredBytes != 350 {
		t.Fatalf("TransferredBytes = %d, want 350", info.TransferredBytes)
	}
	if info.TotalFiles != 4 {
		t.Fatalf("TotalFiles = %d, want 4", info.TotalFiles)
	}
	if info.CompletedFiles != 1 || info.FailedFiles != 1 {
		t.Fatalf("completed=%d failed=%d", info.CompletedFiles, info.FailedFiles)
	}
	if info.Percent <= 0 || info.Percent >= 100 {
		t.Fatalf("Percent = %.2f", info.Percent)
	}
	if info.Speed <= 0 {
		t.Fatalf("Speed = %.2f", info.Speed)
	}
	if callbackInfo.TransferredBytes != info.TransferredBytes {
		t.Fatal("callback did not receive latest progress info")
	}

	text := p.String()
	if !strings.Contains(text, "%") || !strings.Contains(text, "ETA") {
		t.Fatalf("String() = %q", text)
	}
}

func TestAddBytesConcurrent(t *testing.T) {
	p := NewProgress(1000, 1)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.AddBytes(1)
				p.GetInfo()
			}
		}()
	}
	wg.Wait()
	info := p.GetInfo()
	if info.TransferredBytes != 1000 || info.CompletedFiles != 0 || info.FailedFiles != 0 {
		t.Fatalf("incremental progress = %+v", info)
	}
	p.Update(0, true)
	if info = p.GetInfo(); info.TransferredBytes != 1000 || info.CompletedFiles != 1 {
		t.Fatalf("completed progress = %+v", info)
	}
}

func TestProgressZeroTotalBytesDoesNotExplode(t *testing.T) {
	p := NewProgress(0, 0)
	info := p.GetInfo()
	if info.Percent != 0 || info.Speed < 0 {
		t.Fatalf("zero totals info = %#v", info)
	}
	if formatSize(512) != "512 B" || formatSize(2048) != "2.00 KB" {
		t.Fatalf("formatSize outputs unexpected values")
	}
}
