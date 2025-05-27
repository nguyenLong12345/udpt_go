package main

import (
    "bytes"
    "fmt"
    "math/rand"
    "net/http"
    "sync"
    "time"
)

var targetURLs = []string{
    "http://localhost:8080/save",
    "http://localhost:8081/save",
    "http://localhost:8082/save",
}

const (
    numClients    = 1000000
    maxConcurrent = 20
    timeoutLimit  = 3 // Nếu 1 node lỗi liên tiếp 3 lần → coi là down
)

type NodeStats struct {
    Success       int
    Failed        int
    ConsecutiveFails int
    Alive         bool
}

func main() {
    fmt.Println("🚀 Bắt đầu stress test nhiều node, sẽ tự động dừng nếu 1 node chết...")
    start := time.Now()
    rand.Seed(time.Now().UnixNano())

    var wg sync.WaitGroup
    var mu sync.Mutex
    stats := make(map[string]*NodeStats)
    semaphore := make(chan struct{}, maxConcurrent)
    cancelChan := make(chan struct{})
    stopped := false

    // Khoi tao ban dau
    for _, url := range targetURLs {
        stats[url] = &NodeStats{Alive: true}
    }

    for i := 0; i < numClients; i++ {
        select {
        case <-cancelChan:
            fmt.Printf("⛔ Dừng test tại request #%d do node bị down.\n", i)
            goto DONE
        default:
        }

        wg.Add(1)
        semaphore <- struct{}{}

        go func(i int) {
            defer wg.Done()
            defer func() { <-semaphore }()

            name := fmt.Sprintf("user%d", i)
            phone := fmt.Sprintf("09%08d", i)
            data := fmt.Sprintf("name=%s&phone=%s", name, phone)

            // Chọn node theo round-robin
            var target string
            mu.Lock()
            aliveTargets := []string{}
            for _, url := range targetURLs {
                if stats[url].Alive {
                    aliveTargets = append(aliveTargets, url)
                }
            }
            if len(aliveTargets) == 0 {
                if !stopped {
                    close(cancelChan)
                    stopped = true
                }
                mu.Unlock()
                return
            }
            target = aliveTargets[i%len(aliveTargets)]
            mu.Unlock()

            client := &http.Client{Timeout: 2 * time.Second}
            resp, err := client.Post(target, "application/x-www-form-urlencoded", bytes.NewBufferString(data))

            mu.Lock()
            stat := stats[target]
            if err != nil {
                stat.Failed++
                stat.ConsecutiveFails++
                fmt.Printf("❌ Request %d FAILED to %s: %v\n", i, target, err)
                if stat.ConsecutiveFails >= timeoutLimit && stat.Alive {
                    stat.Alive = false
                    fmt.Printf("💥 Node %s bị coi là DEAD (lỗi liên tiếp %d lần)\n", target, timeoutLimit)
                    if !stopped {
                        close(cancelChan)
                        stopped = true
                    }
                }
            } else {
                stat.Success++
                stat.ConsecutiveFails = 0
                resp.Body.Close()
            }
            mu.Unlock()
        }(i)
    }

DONE:
    wg.Wait()
    elapsed := time.Since(start)

    // In báo cáo
    fmt.Println("\n📊 Báo cáo theo node:")
    for _, url := range targetURLs {
        s := stats[url]
        fmt.Printf("🌐 %s\n", url)
        fmt.Printf("   ✅ Thành công: %d\n", s.Success)
        fmt.Printf("   ❌ Thất bại:  %d\n", s.Failed)
        if !s.Alive {
            fmt.Printf("   💀 Trạng thái: DEAD\n")
        }
    }

    fmt.Printf("\n⏱️  Tổng thời gian: %v\n", elapsed)
    fmt.Printf("📈 Trung bình/request: %.2f ms\n", float64(elapsed.Milliseconds())/float64(numClients))
}
