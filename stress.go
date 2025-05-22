package main

import (
    "bytes"
    "fmt"
    "net/http"
    "sync"
    "time"
)

const (
    numClients = 100
    targetURL  = "http://localhost:8001/save"
)

func main() {
    start := time.Now()
    var wg sync.WaitGroup
    for i := 0; i < numClients; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            name := fmt.Sprintf("user%d", i)
            phone := fmt.Sprintf("09%08d", i)
            data := fmt.Sprintf("name=%s&phone=%s", name, phone)
            resp, err := http.Post(targetURL, "application/x-www-form-urlencoded", bytes.NewBufferString(data))
            if err != nil {
                fmt.Printf("❌ Request %d failed: %v\n", i, err)
                return
            }
            resp.Body.Close()
        }(i)
    }
    wg.Wait()
    elapsed := time.Since(start)
    fmt.Printf("✅ Đã gửi %d request trong %v\n", numClients, elapsed)
}
