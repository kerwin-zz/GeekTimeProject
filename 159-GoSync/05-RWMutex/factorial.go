package main

import (
	"fmt"
	"sync"
	"time"
)

func factorial(m *sync.RWMutex, n int) int {
	if n < 1 { // 阶乘退出条件
		return 0
	}
	fmt.Println("RLock")
	m.RLock()
	defer func() {
		fmt.Println("RUnlock")
		m.RUnlock()
	}()
	time.Sleep(100 * time.Millisecond)
	return factorial(m, n-1) * n // 递归调用
}

func main() {
	var mu sync.RWMutex

	// writer，稍微等待，然后知道一个调用Lock的场景
	go func() {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		fmt.Println("Lock")
		time.Sleep(100 * time.Millisecond)
		mu.Unlock()
		fmt.Println("Unlock")
	}()

	go func() {
		factorial(&mu, 10) // 计算10的阶乘，10!
	}()

	select {}
}
