package stm

import (
	"sync"
	"testing"
	"time"
)

func TestDecrement(t *testing.T) {
	x := NewVar(1000)
	for i := 0; i < 500; i++ {
		go Atomically(func(tx *Tx) {
			cur := tx.Get(x).(int)
			tx.Set(x, cur-1)
		})
	}
	done := make(chan struct{})
	go func() {
		for {
			if AtomicGet(x).(int) == 500 {
				break
			}
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("decrement did not complete in time")
	}
}

// read-only transaction aren't exempt from calling tx.verify
func TestReadVerify(t *testing.T) {
	read := make(chan struct{})
	x, y := NewVar(1), NewVar(2)

	// spawn a transaction that writes to x
	go func() {
		<-read
		AtomicSet(x, 3)
		read <- struct{}{}
		// other tx should retry, so we need to read/send again
		read <- <-read
	}()

	// spawn a transaction that reads x, then y. The other tx will modify x in
	// between the reads, causing this tx to retry.
	var x2, y2 int
	Atomically(func(tx *Tx) {
		x2 = tx.Get(x).(int)
		read <- struct{}{}
		<-read // wait for other tx to complete
		y2 = tx.Get(y).(int)
	})
	if x2 == 1 && y2 == 2 {
		t.Fatal("read was not verified")
	}
}

func TestRetry(t *testing.T) {
	x := NewVar(10)
	// spawn 10 transactions, one every 10 milliseconds. This will decrement x
	// to 0 over the course of 100 milliseconds.
	go func() {
		for i := 0; i < 10; i++ {
			time.Sleep(10 * time.Millisecond)
			Atomically(func(tx *Tx) {
				cur := tx.Get(x).(int)
				tx.Set(x, cur-1)
			})
		}
	}()
	// Each time we read x before the above loop has finished, we need to
	// retry. This should result in no more than 1 retry per transaction.
	retry := 0
	Atomically(func(tx *Tx) {
		cur := tx.Get(x).(int)
		if cur != 0 {
			retry++
			tx.Retry()
		}
	})
	if retry > 10 {
		t.Fatal("should have retried at most 10 times, got", retry)
	}
}

func TestVerify(t *testing.T) {
	// tx.verify should check more than pointer equality
	type foo struct {
		i int
	}
	x := NewVar(&foo{3})
	read := make(chan struct{})

	// spawn a transaction that modifies x
	go func() {
		Atomically(func(tx *Tx) {
			<-read
			rx := tx.Get(x).(*foo)
			rx.i = 7
			tx.Set(x, rx)
		})
		read <- struct{}{}
		// other tx should retry, so we need to read/send again
		read <- <-read
	}()

	// spawn a transaction that reads x, then y. The other tx will modify x in
	// between the reads, causing this tx to retry.
	var i int
	Atomically(func(tx *Tx) {
		f := tx.Get(x).(*foo)
		i = f.i
		read <- struct{}{}
		<-read // wait for other tx to complete
	})
	if i == 3 {
		t.Fatal("verify did not retry despite modified Var", i)
	}
}

func TestSelect(t *testing.T) {
	// empty Select should block forever
	c := make(chan struct{})
	go func() {
		Atomically(Select())
		c <- struct{}{}
	}()
	select {
	case <-c:
		t.Fatal("empty Select did not block forever")
	case <-time.After(10 * time.Millisecond):
	}

	// with one arg, Select adds no effect
	x := NewVar(2)
	Atomically(Select(func(tx *Tx) {
		tx.Assert(tx.Get(x).(int) == 2)
	}))

	var picked int
	Atomically(Select(
		// always blocks; should never be selected
		func(tx *Tx) {
			tx.Retry()
			picked = 1
		},
		// always succeeds; should always be selected
		func(tx *Tx) {
			picked = 2
		},
		// always succeeds; should never be selected
		func(tx *Tx) {
			picked = 3
		},
	))
	if picked != 2 {
		t.Fatal("Select selected wrong transaction:", picked)
	}
}

func TestCompose(t *testing.T) {
	nums := make([]int, 100)
	fns := make([]func(*Tx), 100)
	for i := range fns {
		fns[i] = func(x int) func(*Tx) {
			return func(*Tx) { nums[x] = x }
		}(i) // capture loop var
	}
	Atomically(Compose(fns...))
	for i := range nums {
		if nums[i] != i {
			t.Error("Compose failed:", nums[i], i)
		}
	}
}

func TestPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic, got nil")
		}
	}()
	// normal panics should escape Atomically
	Atomically(func(*Tx) {
		panic("foo")
	})
}

func TestReadWritten(t *testing.T) {
	// reading a variable written in the same transaction should return the
	// previously written value
	x := NewVar(3)
	Atomically(func(tx *Tx) {
		tx.Set(x, 5)
		tx.Assert(tx.Get(x).(int) == 5)
	})
}

func TestAtomicSetRetry(t *testing.T) {
	// AtomicSet should cause waiting transactions to retry
	x := NewVar(3)
	done := make(chan struct{})
	go func() {
		Atomically(func(tx *Tx) {
			tx.Assert(tx.Get(x).(int) == 5)
		})
		done <- struct{}{}
	}()
	time.Sleep(10 * time.Millisecond)
	AtomicSet(x, 5)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AtomicSet did not wake up a waiting transaction")
	}
}

func BenchmarkAtomicGet(b *testing.B) {
	x := NewVar(0)
	for i := 0; i < b.N; i++ {
		AtomicGet(x)
	}
}

func BenchmarkAtomicSet(b *testing.B) {
	x := NewVar(0)
	for i := 0; i < b.N; i++ {
		AtomicSet(x, 0)
	}
}

func BenchmarkIncrementSTM(b *testing.B) {
	for i := 0; i < b.N; i++ {
		// spawn 1000 goroutines that each increment x by 1
		x := NewVar(0)
		for i := 0; i < 1000; i++ {
			go Atomically(func(tx *Tx) {
				cur := tx.Get(x).(int)
				tx.Set(x, cur+1)
			})
		}
		// wait for x to reach 1000
		Atomically(func(tx *Tx) {
			tx.Assert(tx.Get(x).(int) == 1000)
		})
	}
}

func BenchmarkIncrementMutex(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var mu sync.Mutex
		x := 0
		for i := 0; i < 1000; i++ {
			go func() {
				mu.Lock()
				x++
				mu.Unlock()
			}()
		}
		for {
			mu.Lock()
			read := x
			mu.Unlock()
			if read == 1000 {
				break
			}
		}
	}
}

func BenchmarkIncrementChannel(b *testing.B) {
	for i := 0; i < b.N; i++ {
		c := make(chan int, 1)
		c <- 0
		for i := 0; i < 1000; i++ {
			go func() {
				c <- 1 + <-c
			}()
		}
		for {
			read := <-c
			if read == 1000 {
				break
			}
			c <- read
		}
	}
}

func BenchmarkReadVarSTM(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(1000)
		x := NewVar(0)
		for i := 0; i < 1000; i++ {
			go func() {
				AtomicGet(x)
				wg.Done()
			}()
		}
		wg.Wait()
	}
}

func BenchmarkReadVarMutex(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(1000)
		x := 0
		for i := 0; i < 1000; i++ {
			go func() {
				mu.Lock()
				_ = x
				mu.Unlock()
				wg.Done()
			}()
		}
		wg.Wait()
	}
}

func BenchmarkReadVarChannel(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(1000)
		c := make(chan int)
		close(c)
		for i := 0; i < 1000; i++ {
			go func() {
				<-c
				wg.Done()
			}()
		}
		wg.Wait()
	}
}
