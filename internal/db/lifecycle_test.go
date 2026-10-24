package db

import (
	"testing"
	"time"
)

const lifecycleWait = 2 * time.Second

func TestLifecycleWriterWaitsForReaders(t *testing.T) {
	LockRead()
	writerDone := make(chan struct{})
	go func() {
		LockWrite()
		close(writerDone)
		UnlockWrite()
	}()

	select {
	case <-writerDone:
		UnlockRead()
		t.Fatal("write lock acquired while a reader was active")
	case <-time.After(50 * time.Millisecond):
	}

	UnlockRead()
	select {
	case <-writerDone:
	case <-time.After(lifecycleWait):
		t.Fatal("write lock not acquired after the reader released")
	}
}

func TestLifecycleWriterBlocksNewReaders(t *testing.T) {
	LockWrite()
	readerDone := make(chan struct{})
	go func() {
		LockRead()
		close(readerDone)
		UnlockRead()
	}()

	select {
	case <-readerDone:
		UnlockWrite()
		t.Fatal("read lock acquired while the writer was active")
	case <-time.After(50 * time.Millisecond):
	}

	UnlockWrite()
	select {
	case <-readerDone:
	case <-time.After(lifecycleWait):
		t.Fatal("reader not admitted after the writer released")
	}
}

func TestLifecycleAllowsConcurrentReaders(t *testing.T) {
	const readers = 8
	acquired := make(chan struct{}, readers)
	release := make(chan struct{})
	for i := 0; i < readers; i++ {
		go func() {
			LockRead()
			acquired <- struct{}{}
			<-release
			UnlockRead()
		}()
	}
	for i := 0; i < readers; i++ {
		select {
		case <-acquired:
		case <-time.After(lifecycleWait):
			close(release)
			t.Fatalf("only %d of %d readers acquired the shared lock", i, readers)
		}
	}
	close(release)
}
