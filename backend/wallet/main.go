package wallet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// --- 配置区域 ---
const SignerServiceURL = "http://127.0.0.1:8081/internal/keys/create"
const DSN = "root:123456@tcp(127.0.0.1:3306)/wallet?charset=utf8mb4&parseTime=True&loc=Local"

// --- 数据库模型 (业务层) ---
type UserAccount struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    string `gorm:"index"` // 例如 "user_001"
	Address   string `gorm:"type:char(42)"`
	CreatedAt time.Time
}

var db *gorm.DB

func main() {
	var err error
	db, err = gorm.Open(mysql.Open(DSN), &gorm.Config{})
	if err != nil {
		panic("无法连接业务数据库")
	}
	db.AutoMigrate(&UserAccount{})

	r := gin.Default()

	// 对外接口：为用户分配充值地址
	r.POST("/api/v1/user/allocate_address", allocateAddressHandler)

	r.Run(":8080") // 业务服务运行在 8080
}

// --- 核心逻辑 ---

type AllocateReq struct {
	UserID string `json:"user_id" binding:"required"`
}

type SignerResponse struct {
	Address string `json:"address"`
	Error   string `json:"error"`
}

func allocateAddressHandler(c *gin.Context) {
	var req AllocateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "Invalid params"})
		return
	}

	// 1. 检查该用户是否已有地址 (假设每个用户每个链只有一个充值地址)
	var exist UserAccount
	if err := db.Where("user_id = ?", req.UserID).First(&exist).Error; err == nil {
		c.JSON(200, gin.H{"code": 0, "address": exist.Address, "msg": "Existing address returned"})
		return
	}

	// 2. 远程调用 Signer 服务，请求生成一个新地址
	// 注意：这里没有生成私钥，私钥在 Signer 那边
	address, err := callSignerToCreateKey()
	if err != nil {
		c.JSON(500, gin.H{"error": "Failed to generate address from signer"})
		return
	}

	// 3. 将 UserID <-> Address 关系写入业务数据库
	newAccount := UserAccount{
		UserID:  req.UserID,
		Address: address,
	}
	if err := db.Create(&newAccount).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to save account"})
		return
	}

	c.JSON(200, gin.H{
		"code":    0,
		"user_id": req.UserID,
		"address": address,
	})
}

// 模拟 HTTP Client 调用
func callSignerToCreateKey() (string, error) {
	resp, err := http.Post(SignerServiceURL, "application/json", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("signer error code: %d", resp.StatusCode)
	}

	var res SignerResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}
	return res.Address, nil
}
