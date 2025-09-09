package monitor_test

import (
	"log"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/thanhpk/randstr"
)

func TestNewBus(t *testing.T) {
	expected := 40000
	bus := make(chan string)
	cnt := 0
	wg := sync.WaitGroup{}
	locker := sync.Mutex{}

	fn := func(receivername string) {
		wg.Add(1)
		r := 0
		log.Println(receivername + " started.")
		for range bus {
			// log.Printf("%s: %s", receivername, v)
			// cnt++
			r++
		}
		log.Printf("%s total %d", receivername, r)
		locker.Lock()
		cnt += r
		locker.Unlock()
		wg.Done()
	}
	go fn("receiver1")
	go fn("receiver2")
	go fn("receiver3")
	go fn("receiver4")

	time.Sleep(time.Millisecond * 50)

	for i := 0; i < expected; i++ {
		bus <- randstr.Hex(16)
		// time.Sleep(time.Nanosecond * 10)
	}
	close(bus)
	// time.Sleep(1 * time.Second)
	wg.Wait()
	assert.Equal(t, expected, cnt)
}

func TestOne2Many(t *testing.T) {
	expected := 10000

	bus := make(chan string)
	// cnt := 0
	wg := sync.WaitGroup{}
	locker := sync.Mutex{}

	receivers := make([]chan string, 0)

	reg := func() chan string {
		locker.Lock()
		defer locker.Unlock()
		r := make(chan string)
		receivers = append(receivers, r)
		return r
	}

	fn := func(receivername string) {
		wg.Add(1)
		r := 0
		c := reg()

		log.Println(receivername + " started.")
		for v := range c {
			log.Printf("%s: %s", receivername, v)
			r++
		}
		log.Printf("%s total %d", receivername, r)

		assert.Equal(t, expected, r)

		wg.Done()
	}
	go fn("receiver1")
	go fn("receiver2")
	go fn("receiver3")
	go fn("receiver4")
	go fn("receiver5")

	fnEngine := func() {
		for v := range bus {
			for _, c := range receivers {
				c <- v
			}
		}
		for _, c := range receivers {
			close(c)
		}
	}
	go fnEngine()

	time.Sleep(10 * time.Millisecond)

	for i := 0; i < expected; i++ {
		bus <- randstr.Hex(16)
		// time.Sleep(time.Nanosecond * 10)
	}
	close(bus)
	// time.Sleep(1 * time.Second)
	wg.Wait()
	// assert.Equal(t, expected*4, cnt)
}
