package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gbjohnso/gosync/pkg/transport"
)

type checksumModeTransport struct {
	fakeTransport
	enabled bool
	events  []string
}

var _ transport.ChecksumConfigurer = (*checksumModeTransport)(nil)

func (f *checksumModeTransport) SetChecksum(enabled bool) {
	f.events = append(f.events, fmt.Sprintf("checksum=%t", enabled))
	f.enabled = enabled
}

func (f *checksumModeTransport) Connect(host string, port int) error {
	f.events = append(f.events, fmt.Sprintf("connect/checksum=%t", f.enabled))
	return f.fakeTransport.Connect(host, port)
}

func TestRunChecksumConfiguredBeforeConnect(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, failConnect := range []bool{false, true} {
			t.Run(fmt.Sprintf("checksum=%t/connectFailure=%t", enabled, failConnect), func(t *testing.T) {
				source := filepath.Join(t.TempDir(), "payload")
				if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
					t.Fatal(err)
				}
				// Start with the opposite setting so a missing false assignment fails too.
				tr := &checksumModeTransport{enabled: !enabled}
				if failConnect {
					tr.connectErr = errors.New("intentional connect failure")
				}
				err := NewSync(source, "host:1234:/target", tr, Config{Workers: 1, Quiet: true, Checksum: enabled}).Run()
				if failConnect {
					if !errors.Is(err, tr.connectErr) || len(tr.sent) != 0 {
						t.Fatalf("Run() = %v, sent = %v", err, tr.sent)
					}
				} else if err != nil || len(tr.sent) != 1 {
					t.Fatalf("Run() = %v, sent = %v", err, tr.sent)
				}
				want := []string{fmt.Sprintf("checksum=%t", enabled), fmt.Sprintf("connect/checksum=%t", enabled)}
				if !reflect.DeepEqual(tr.events, want) {
					t.Fatalf("events = %v, want %v", tr.events, want)
				}
			})
		}
	}
}

func TestRunChecksumConfigurerOptional(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			tr := &fakeTransport{}
			if err := NewSync(t.TempDir(), "host:/target", tr, Config{Workers: 1, Quiet: true, Checksum: enabled}).Run(); err != nil {
				t.Fatal(err)
			}
			if tr.connectHost != "host" {
				t.Fatal("transport without ChecksumConfigurer was not connected")
			}
		})
	}
}

func TestRunChecksumDryRunDoesNotConfigureOrConnect(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			tr := &checksumModeTransport{enabled: !enabled}
			if err := NewSync(t.TempDir(), "host:/target", tr, Config{Workers: 1, Quiet: true, DryRun: true, Checksum: enabled}).Run(); err != nil {
				t.Fatal(err)
			}
			if len(tr.events) != 0 || tr.enabled != !enabled {
				t.Fatalf("dry run changed transport: events=%v checksum=%t", tr.events, tr.enabled)
			}
		})
	}
}
