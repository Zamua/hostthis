package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type testDrain struct {
	entered chan struct{}
	release <-chan struct{}
}

func (d *testDrain) Shutdown(ctx context.Context) error {
	close(d.entered)
	select {
	case <-d.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type testRelay struct {
	*testDrain
	stopped bool
}

func (r *testRelay) StopAdmission() { r.stopped = true }

func (r *testRelay) Shutdown(ctx context.Context) error {
	if !r.stopped {
		return errors.New("relay drained before admission stopped")
	}
	return r.testDrain.Shutdown(ctx)
}

type testSSH struct {
	shutdownEntered chan struct{}
	shutdownRelease <-chan struct{}
	closeEntered    chan struct{}
	closeRelease    <-chan struct{}
}

func (s *testSSH) Shutdown(ctx context.Context) error {
	close(s.shutdownEntered)
	select {
	case <-s.shutdownRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *testSSH) Close() error {
	close(s.closeEntered)
	<-s.closeRelease
	return nil
}

func closedSignal() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// immediate is a drain that returns as soon as it is entered.
func immediate() *testDrain {
	return &testDrain{entered: make(chan struct{}), release: closedSignal()}
}

// newTestSSH parks Shutdown until shutdownRelease and Close until closeRelease.
func newTestSSH(shutdownRelease, closeRelease <-chan struct{}) *testSSH {
	return &testSSH{
		shutdownEntered: make(chan struct{}), shutdownRelease: shutdownRelease,
		closeEntered: make(chan struct{}), closeRelease: closeRelease,
	}
}

func awaitSignal(t *testing.T, name string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestShutdownStopsAdmissionBeforeConcurrentDrains(t *testing.T) {
	release := make(chan struct{})
	public := &testDrain{entered: make(chan struct{}), release: release}
	metrics := &testDrain{entered: make(chan struct{}), release: release}
	relay := &testRelay{testDrain: &testDrain{entered: make(chan struct{}), release: release}}
	ssh := newTestSSH(release, closedSignal())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- shutdownDaemon(ctx, 500*time.Millisecond, public, metrics, relay, ssh, func() {}, func() {})
	}()

	awaitSignal(t, "public HTTP drain", public.entered)
	awaitSignal(t, "metrics drain", metrics.entered)
	awaitSignal(t, "relay drain", relay.entered)
	awaitSignal(t, "SSH drain", ssh.shutdownEntered)
	if !relay.stopped {
		t.Fatal("relay drain started before admission stopped")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestShutdownWaitsForSSHThenFinalizersThenCleanup(t *testing.T) {
	sshRelease := make(chan struct{})
	finalRelease := make(chan struct{})
	finalStarted := make(chan struct{})
	cleanupStarted := make(chan struct{})
	ssh := newTestSSH(sshRelease, closedSignal())
	relay := &testRelay{testDrain: immediate()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- shutdownDaemon(ctx, 500*time.Millisecond, immediate(), immediate(), relay, ssh, func() {
			close(finalStarted)
			<-finalRelease
		}, func() { close(cleanupStarted) })
	}()

	awaitSignal(t, "SSH drain", ssh.shutdownEntered)
	select {
	case <-finalStarted:
		t.Fatal("finalizers started while SSH could still create one")
	default:
	}
	close(sshRelease)
	awaitSignal(t, "finalizers", finalStarted)
	select {
	case <-cleanupStarted:
		t.Fatal("blob cleanup started before finalizers completed")
	default:
	}
	close(finalRelease)
	awaitSignal(t, "blob cleanup", cleanupStarted)
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-ssh.closeEntered:
		t.Fatal("graceful SSH shutdown called force close")
	default:
	}
}

func TestShutdownForceClosesSSHBeforeWaitingFinalizers(t *testing.T) {
	closeRelease := make(chan struct{})
	finalStarted := make(chan struct{})
	ssh := newTestSSH(make(chan struct{}), closeRelease)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- shutdownDaemon(ctx, 20*time.Millisecond, immediate(), immediate(),
			&testRelay{testDrain: immediate()}, ssh,
			func() { close(finalStarted) }, func() {})
	}()

	awaitSignal(t, "SSH force close", ssh.closeEntered)
	select {
	case <-finalStarted:
		t.Fatal("finalizers started before force close returned")
	default:
	}
	close(closeRelease)
	awaitSignal(t, "finalizers", finalStarted)
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestShutdownReturnsAtHardDeadline(t *testing.T) {
	finalRelease := make(chan struct{})
	ssh := newTestSSH(closedSignal(), closedSignal())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := shutdownDaemon(ctx, time.Second, immediate(), immediate(),
		&testRelay{testDrain: immediate()}, ssh,
		func() { <-finalRelease }, func() {})
	close(finalRelease)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("hard deadline returned after %s", elapsed)
	}
}

func TestDaemonExitsCleanlyOnSIGTERM(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestDaemonProcess$")
	cmd.Env = append(os.Environ(),
		"HOSTTHIS_DAEMON_PROCESS=1",
		"HOSTTHIS_TEST_DATA_DIR="+t.TempDir(),
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()

	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "http: listening") {
				close(ready)
				return
			}
		}
	}()

	select {
	case <-ready:
	case err := <-finished:
		t.Fatalf("daemon exited before startup: %v", err)
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-finished
		t.Fatal("daemon did not start")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal daemon: %v", err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("daemon shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-finished
		t.Fatal("daemon did not stop")
	}
}

func TestDaemonProcess(t *testing.T) {
	if os.Getenv("HOSTTHIS_DAEMON_PROCESS") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("hostthisd-test", flag.ExitOnError)
	os.Args = []string{
		"hostthisd",
		"--data-dir", os.Getenv("HOSTTHIS_TEST_DATA_DIR"),
		"--ssh-addr", "127.0.0.1:0",
		"--http-addr", "127.0.0.1:0",
		"--metrics-addr", "127.0.0.1:0",
		"--apex-domain", "example.test",
		"--landing", filepath.Join(t.TempDir(), "missing.html"),
	}
	main()
}
