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

// 提现请求参数
type WithdrawReq struct {
	UserID    string  `json:"user_id" binding:"required"`
	ToAddress string  `json:"to_address" binding:"required"`
	Amount    float64 `json:"amount" binding:"required,gt=0"`
}

// --- 1. 补充定义 UserWithdrawal 结构体 ---
// 必须定义这个结构体，GORM 才能把 Go 的数据映射到数据库表
type UserWithdrawal struct {
	ID        uint    `gorm:"primaryKey"`
	UserID    string  `gorm:"type:varchar(64);not null"`
	ToAddress string  `gorm:"type:char(42);not null"`
	Amount    float64 `gorm:"type:decimal(36,18);not null"`
	TxHash    string  `gorm:"type:char(66)"`
	Status    int     `gorm:"default:0"` // 0:待处理, 1:处理中, 2:成功, 3:失败
	CreatedAt time.Time
	UpdatedAt time.Time
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

	// 添加提现路由
	r.POST("/api/v1/user/withdraw", withdrawHandler)

	r.Run(":8080") // 业务服务运行在 8080
}

func withdrawHandler(c *gin.Context) {
	var req WithdrawReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "参数错误"})
		return
	}

	// --- 开启事务 ---
	tx := db.Begin()
	defer tx.Rollback()

	// --- 2. 修复 RowsAffected 错误 ---
	// 执行扣款 SQL
	result := tx.Exec("UPDATE user_accounts SET balance = balance - ? WHERE user_id = ? AND balance >= ?", req.Amount, req.UserID, req.Amount)

	if result.Error != nil {
		c.JSON(500, gin.H{"error": "数据库错误"})
		return
	}

	// ❌ 之前的错误写法: rowsAffected, _ := result.RowsAffected()
	// ✅ 正确写法: 直接访问字段
	if result.RowsAffected == 0 {
		c.JSON(400, gin.H{"error": "余额不足或用户不存在"})
		return
	}

	// --- 3. 创建提现记录 (使用 GORM 的 Create 方法更安全) ---
	withdrawRecord := UserWithdrawal{
		UserID:    req.UserID,
		ToAddress: req.ToAddress,
		Amount:    req.Amount,
		Status:    0, // 0 代表待处理
	}

	// 使用 GORM 的 Create 方法，而不是手写 INSERT SQL
	if err := tx.Create(&withdrawRecord).Error; err != nil {
		c.JSON(500, gin.H{"error": "创建提现订单失败"})
		return
	}

	// --- 提交事务 ---
	if err := tx.Commit().Error; err != nil {
		c.JSON(500, gin.H{"error": "事务提交失败"})
		return
	}

	c.JSON(200, gin.H{
		"msg":      "提现申请已提交",
		"amount":   req.Amount,
		"order_id": withdrawRecord.ID, // 返回生成的订单ID
	})
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
