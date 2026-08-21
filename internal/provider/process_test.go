//go:build !windows

package provider

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStartCommandWritesSubmittedCodeToStdin(t *testing.T) {
	process, err := StartCommand(context.Background(), os.Args[0], []string{"-test.run=TestProcessOwnerHelper"}, append(os.Environ(), "GO_WANT_CODE_SUBMISSION_HELPER=1"))
	if err != nil {
		t.Fatal(err)
	}
	linesDone := make(chan []string, 1)
	go func() {
		scanner := bufio.NewScanner(process.Output())
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		linesDone <- lines
	}()
	if err := process.(CodeSubmitter).SubmitCode("SYNTHETIC-CODE"); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	lines := <-linesDone
	if len(lines) == 0 || lines[0] != "received" {
		t.Fatalf("output = %q", lines)
	}
}

func TestStartCommandLeavesTerminationToProcessOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	process, err := StartCommand(ctx, os.Args[0], []string{"-test.run=TestProcessOwnerHelper"}, append(os.Environ(), "GO_WANT_PROCESS_OWNER_HELPER=1"))
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(process.Output())
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("first output = %q", scanner.Text())
	}
	var waitErr error
	waitDone := make(chan struct{})
	var once sync.Once
	go func() {
		waitErr = process.Wait()
		once.Do(func() { close(waitDone) })
	}()
	cancel()
	select {
	case <-waitDone:
		t.Fatalf("context cancellation stopped child: %v", waitErr)
	case <-time.After(50 * time.Millisecond):
	}
	if err := process.Terminate(); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || scanner.Text() != "interrupted" {
		t.Fatalf("interrupt output = %q", scanner.Text())
	}
	select {
	case <-waitDone:
		t.Fatalf("interrupt reaped child before kill: %v", waitErr)
	case <-time.After(50 * time.Millisecond):
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("child did not exit after manager-owned kill")
	}
	if waitErr == nil {
		t.Fatal("killed child returned a successful exit")
	}
}

func TestProcessOwnerHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CODE_SUBMISSION_HELPER") == "1" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil || line != "SYNTHETIC-CODE\n" {
			t.Fatalf("submitted input was not the synthetic code")
		}
		fmt.Println("received")
		return
	}
	if os.Getenv("GO_WANT_PROCESS_OWNER_HELPER") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	fmt.Println("ready")
	<-signals
	fmt.Println("interrupted")
	select {}
}
