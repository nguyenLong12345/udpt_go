package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "hash/fnv"
    "io"
    "net/http"
    "os"
    "strings"
    "time"

    "github.com/gin-gonic/gin"
    "github.com/syndtr/goleveldb/leveldb"
)

type Contact struct {
    Name  string `json:"name"`
    Phone string `json:"phone"`
}

type EditRequest struct {
    OriginalName string `json:"original_name"`
    Name         string `json:"name"`
    Phone        string `json:"phone"`
}

var (
    db       *leveldb.DB
    thisPort string
    nodeList []string
    thisNode string
)

func main() {
    thisPort = os.Getenv("PORT")
    nodeList = strings.Split(os.Getenv("NODES"), ",")
    thisNode = "http://localhost:" + thisPort

    dbPath := fmt.Sprintf("data/db-%s", thisPort)
    os.MkdirAll("data", 0755)
    var err error
    db, err = leveldb.OpenFile(dbPath, nil)
    if err != nil {
        panic(err)
    }
    defer db.Close()

    r := gin.Default()

    r.LoadHTMLGlob("templates/*.html")
    r.Static("/static", "./static")

    r.GET("/", showContacts)
    r.POST("/delete_remote", handleDeleteRemote)
    r.POST("/add", handleAdd)
    r.POST("/delete", handleDelete)
    r.POST("/edit", handleEdit)
    r.POST("/replicate", handleReplication)
    r.POST("/replicate_edit", handleEditReplication)

    // API đơn giản để ping kiểm tra node còn sống
    r.GET("/ping", func(c *gin.Context) {
        c.String(200, "pong")
    })

    // Chạy goroutine đồng bộ dữ liệu pending khi primary hồi phục
    go syncPendingDataLoop()

    r.Run(":" + thisPort)
}

// ========== Sharding ===========
func getPrimaryNode(name string) string {
    h := fnv.New32a()
    h.Write([]byte(name))
    return nodeList[int(h.Sum32())%len(nodeList)]
}

func getBackupNodes(primary string) []string {
    var backups []string
    for _, node := range nodeList {
        if node != primary && node != thisNode {
            backups = append(backups, node)
        }
    }
    return backups
}

// ========== Routes =============
func showContacts(c *gin.Context) {
    iter := db.NewIterator(nil, nil)
    var contacts []Contact
    for iter.Next() {
        key := string(iter.Key())
        if strings.HasPrefix(key, "pending_") {
            // Bỏ qua dữ liệu pending khi hiển thị
            continue
        }
        contacts = append(contacts, Contact{
            Name:  key,
            Phone: string(iter.Value()),
        })
    }
    iter.Release()
    c.HTML(http.StatusOK, "index.html", gin.H{"contacts": contacts, "port": thisPort})
}

func handleAdd(c *gin.Context) {
    name := c.PostForm("name")
    phone := c.PostForm("phone")
    contact := Contact{Name: name, Phone: phone}
    primary := getPrimaryNode(name)

    if primary != thisNode {
        err := forwardFormToNode(primary, "/add", contact)
        if err != nil {
            // Primary node unreachable, lưu tạm dưới key pending_
            fmt.Println("⚠️ Primary node unreachable, saving locally as fallback.")
            key := "pending_" + name
            if err := db.Put([]byte(key), []byte(phone), nil); err != nil {
                fmt.Println("❌ Lỗi lưu dữ liệu pending:", err)
            }
        }
        c.Redirect(http.StatusFound, "/")
        return
    }

    // Đây là primary node → lưu dữ liệu
    if err := db.Put([]byte(name), []byte(phone), nil); err != nil {
        c.String(500, "Lỗi khi lưu dữ liệu: %v", err)
        return
    }

    // Replicate sang các backup node
    for _, backup := range getBackupNodes(primary) {
        forwardJSONToNode(backup, "/replicate", contact)
    }

    c.Redirect(http.StatusFound, "/")
}

func handleDelete(c *gin.Context) {
    name := c.PostForm("name")

    // Xóa local
    if err := db.Delete([]byte(name), nil); err != nil {
        fmt.Println("❌ Lỗi xóa dữ liệu:", err)
    }

    // Gửi yêu cầu xóa đến các node khác
    for _, node := range nodeList {
        if node != thisNode {
            go sendDeleteToNode(node, name)
        }
    }

    c.Redirect(http.StatusFound, "/")
}

func handleDeleteRemote(c *gin.Context) {
    name := c.PostForm("name")
    if err := db.Delete([]byte(name), nil); err != nil {
        fmt.Println("❌ Lỗi xóa từ xa:", err)
    }
    c.String(200, "deleted")
}

func sendDeleteToNode(nodeURL string, name string) {
    client := &http.Client{Timeout: 2 * time.Second}
    data := fmt.Sprintf("name=%s", name)
    resp, err := client.Post(nodeURL+"/delete_remote", "application/x-www-form-urlencoded", strings.NewReader(data))
    if err != nil {
        fmt.Printf("⚠️ Không gửi được DELETE đến %s: %v\n", nodeURL, err)
        return
    }
    defer resp.Body.Close()
    _, _ = io.ReadAll(resp.Body)
}

func handleEdit(c *gin.Context) {
    originalName := c.PostForm("original_name")
    newName := c.PostForm("name")
    phone := c.PostForm("phone")
    
    editReq := EditRequest{
        OriginalName: originalName,
        Name:         newName,
        Phone:        phone,
    }

    // Xác định primary node cho cả tên cũ và tên mới
    originalPrimary := getPrimaryNode(originalName)
    newPrimary := getPrimaryNode(newName)

    // Trường hợp 1: Tên không đổi hoặc primary node giống nhau
    if originalName == newName || originalPrimary == newPrimary {
        if originalPrimary != thisNode {
            err := forwardEditToNode(originalPrimary, editReq)
            if err != nil {
                fmt.Println("⚠️ Không thể gửi sửa đến node chính, bỏ qua")
            }
            c.Redirect(http.StatusFound, "/")
            return
        }

        // Đây là primary node → xóa dữ liệu cũ trước, sau đó thêm dữ liệu mới
        // Luôn xóa bản ghi cũ trước khi cập nhật (dù tên có thay đổi hay không)
        if err := db.Delete([]byte(originalName), nil); err != nil {
            fmt.Printf("⚠️ Không thể xóa bản ghi cũ '%s': %v\n", originalName, err)
        }
        
        // Thêm bản ghi mới
        if err := db.Put([]byte(newName), []byte(phone), nil); err != nil {
            c.String(500, "Lỗi khi sửa dữ liệu: %v", err)
            return
        }

        // Replicate việc chỉnh sửa sang các backup node
        for _, backup := range getBackupNodes(originalPrimary) {
            forwardEditJSONToNode(backup, "/replicate_edit", editReq)
        }
    } else {
        // Trường hợp 2: Tên thay đổi và primary node khác nhau
        // Bước 1: Xóa khỏi primary node cũ và backup nodes của nó
        if originalPrimary != thisNode {
            // Gửi yêu cầu xóa đến primary node cũ
            go sendDeleteToNode(originalPrimary, originalName)
        } else {
            // Xóa local nếu đây là primary node cũ
            db.Delete([]byte(originalName), nil)
        }
        
        // Xóa khỏi tất cả backup nodes của primary cũ
        for _, backup := range getBackupNodes(originalPrimary) {
            go sendDeleteToNode(backup, originalName)
        }

        // Bước 2: Thêm vào primary node mới
        newContact := Contact{Name: newName, Phone: phone}
        if newPrimary != thisNode {
            err := forwardFormToNode(newPrimary, "/add", newContact)
            if err != nil {
                // Primary node mới không khả dụng, lưu tạm
                fmt.Println("⚠️ Primary node mới không khả dụng, lưu tạm")
                key := "pending_" + newName
                if err := db.Put([]byte(key), []byte(phone), nil); err != nil {
                    fmt.Println("❌ Lỗi lưu dữ liệu pending:", err)
                }
            }
        } else {
            // Đây là primary node mới → lưu local và replicate
            if err := db.Put([]byte(newName), []byte(phone), nil); err != nil {
                c.String(500, "Lỗi khi lưu dữ liệu mới: %v", err)
                return
            }
            
            // Replicate sang backup nodes của primary mới
            for _, backup := range getBackupNodes(newPrimary) {
                forwardJSONToNode(backup, "/replicate", newContact)
            }
        }
    }

    c.Redirect(http.StatusFound, "/")
}

func handleReplication(c *gin.Context) {
    var contact Contact
    if err := c.BindJSON(&contact); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": "Dữ liệu không hợp lệ"})
        return
    }
    if err := db.Put([]byte(contact.Name), []byte(contact.Phone), nil); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi lưu dữ liệu replicate"})
        return
    }
    c.JSON(http.StatusOK, gin.H{"status": "replicated"})
}

func handleEditReplication(c *gin.Context) {
    var editReq EditRequest
    if err := c.BindJSON(&editReq); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": "Dữ liệu không hợp lệ"})
        return
    }

    // Luôn xóa bản ghi cũ trước (dù tên có thay đổi hay không)
    if err := db.Delete([]byte(editReq.OriginalName), nil); err != nil {
        fmt.Printf("⚠️ Không thể xóa bản ghi cũ '%s' trong replication: %v\n", editReq.OriginalName, err)
    }

    // Thêm bản ghi mới
    if err := db.Put([]byte(editReq.Name), []byte(editReq.Phone), nil); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi cập nhật dữ liệu"})
        return
    }

    c.JSON(http.StatusOK, gin.H{"status": "edit replicated"})
}

// ===== Forward utils =======
func forwardFormToNode(nodeURL, path string, contact Contact) error {
    data := fmt.Sprintf("name=%s&phone=%s", contact.Name, contact.Phone)
    client := &http.Client{Timeout: 3 * time.Second}
    resp, err := client.Post(nodeURL+path, "application/x-www-form-urlencoded", strings.NewReader(data))
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    _, _ = io.ReadAll(resp.Body)
    return nil
}

func forwardEditToNode(nodeURL string, editReq EditRequest) error {
    data := fmt.Sprintf("original_name=%s&name=%s&phone=%s", editReq.OriginalName, editReq.Name, editReq.Phone)
    client := &http.Client{Timeout: 3 * time.Second}
    resp, err := client.Post(nodeURL+"/edit", "application/x-www-form-urlencoded", strings.NewReader(data))
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    _, _ = io.ReadAll(resp.Body)
    return nil
}

func forwardJSONToNode(nodeURL, path string, contact Contact) {
    jsonData, _ := json.Marshal(contact)
    client := &http.Client{Timeout: 3 * time.Second}
    resp, err := client.Post(nodeURL+path, "application/json", bytes.NewBuffer(jsonData))
    if err != nil {
        fmt.Printf("⚠️ Lỗi replicate đến %s: %v\n", nodeURL, err)
        return
    }
    defer resp.Body.Close()
    _, _ = io.ReadAll(resp.Body)
}

func forwardEditJSONToNode(nodeURL, path string, editReq EditRequest) {
    jsonData, _ := json.Marshal(editReq)
    client := &http.Client{Timeout: 3 * time.Second}
    resp, err := client.Post(nodeURL+path, "application/json", bytes.NewBuffer(jsonData))
    if err != nil {
        fmt.Printf("⚠️ Lỗi replicate edit đến %s: %v\n", nodeURL, err)
        return
    }
    defer resp.Body.Close()
    _, _ = io.ReadAll(resp.Body)
}

// ===== Fault Tolerance - Đồng bộ lại dữ liệu pending ======
func syncPendingDataLoop() {
    for {
        time.Sleep(10 * time.Second)

        iter := db.NewIterator(nil, nil)
        for iter.Next() {
            key := string(iter.Key())
            if strings.HasPrefix(key, "pending_") {
                name := strings.TrimPrefix(key, "pending_")
                phone := string(iter.Value())
                contact := Contact{Name: name, Phone: phone}
                primary := getPrimaryNode(name)

                // Nếu primary sống và không phải chính mình
                if isNodeAlive(primary) && primary != thisNode {
                    err := forwardFormToNode(primary, "/add", contact)
                    if err == nil {
                        // Gửi thành công → xóa pending
                        if err := db.Delete([]byte(key), nil); err != nil {
                            fmt.Println("❌ Lỗi xóa dữ liệu pending:", err)
                        } else {
                            fmt.Printf("✅ Đồng bộ lại '%s' thành công đến %s\n", name, primary)
                        }
                    }
                }
            }
        }
        iter.Release()
    }
}

func isNodeAlive(nodeURL string) bool {
    client := http.Client{Timeout: 2 * time.Second}
    resp, err := client.Get(nodeURL + "/ping")
    if err != nil {
        return false
    }
    defer resp.Body.Close()
    return resp.StatusCode == http.StatusOK
}