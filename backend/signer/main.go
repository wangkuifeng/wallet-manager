package signer

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 实现 signer (签名机服务)
// --- 配置区域 ---
// 真正生产环境中，这个 MasterKey 应该在启动时从环境变量或 Vault 读取，绝对不能写死在代码里
const SignerMasterKey = "12345678901234567890123456789012" // 32字节
const DSN = "root:123456@tcp(127.0.0.1:3306)/secure_signer?charset=utf8mb4&parseTime=True&loc=Local"

// --- 数据库模型 ---
type KeyStore struct {
	ID                  uint   `gorm:"primaryKey"`
	Address             string `gorm:"uniqueIndex;type:char(42)"`
	EncryptedPrivateKey string `gorm:"type:text"`
}

var db *gorm.DB

func main() {
	var err error
	db, err = gorm.Open(mysql.Open(DSN), &gorm.Config{})
	if err != nil {
		panic("无法连接签名机数据库: " + err.Error())
	}
	db.AutoMigrate(&KeyStore{})

	r := gin.Default()

	// 对内接口：生成新地址
	r.POST("/internal/keys/create", createKeyHandler)

	//生成签名
	r.POST("/internal/sign_tx", signTxHandler)

	// 注意：Signer 应该运行在内网，不暴露公网端口
	r.Run(":8081")
}

// --- 核心逻辑 ---

func createKeyHandler(c *gin.Context) {
	// 1. 生成以太坊私钥
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		c.JSON(500, gin.H{"error": "Key generation failed"})
		return
	}

	// 2. 推导地址
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()

	// 3. 加密私钥 (AES-GCM)
	privBytes := crypto.FromECDSA(privateKey)
	privHex := hex.EncodeToString(privBytes)
	encryptedKey, err := encrypt(privHex, SignerMasterKey)
	if err != nil {
		c.JSON(500, gin.H{"error": "Encryption failed"})
		return
	}

	// 4. 落库 (存入 Signer 自己的数据库)
	keyRecord := KeyStore{
		Address:             address,
		EncryptedPrivateKey: encryptedKey,
	}
	if err := db.Create(&keyRecord).Error; err != nil {
		c.JSON(500, gin.H{"error": "Database error"})
		return
	}

	// 5. 只返回地址，不返回私钥！
	c.JSON(200, gin.H{
		"address": address,
	})
}

// AES-GCM 加密工具
func encrypt(plaintext, keyStr string) (string, error) {
	block, err := aes.NewCipher([]byte(keyStr))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

// 请求体
type SignTxReq struct {
	Address string `json:"address"` // 使用哪个地址的私钥签名
	TxRLP   string `json:"tx_rlp"`  // 待签名的交易数据 (RLP编码的十六进制)
	ChainID int64  `json:"chain_id"`
}

// Signer 服务的升级 (支持交易签名)
func signTxHandler(c *gin.Context) {
	var req SignTxReq
	c.ShouldBindJSON(&req)

	// 1. 从数据库查找加密私钥
	var keyRecord KeyStore
	if err := db.Where("address = ?", req.Address).First(&keyRecord).Error; err != nil {
		c.JSON(404, gin.H{"error": "Key not found"})
		return
	}

	// 2. 解密私钥
	privKeyStr, _ := decrypt(keyRecord.EncryptedPrivateKey, SignerMasterKey)
	privKey, _ := crypto.HexToECDSA(privKeyStr)

	// 3. 解析交易
	tx := new(types.Transaction)
	rlpBytes, _ := hex.DecodeString(req.TxRLP)
	rlp.DecodeBytes(rlpBytes, tx)

	// 4. 签名
	signer := types.NewEIP155Signer(big.NewInt(req.ChainID))
	signedTx, _ := types.SignTx(tx, signer, privKey)

	// 5. 返回签名后的完整交易数据
	txBytes, _ := signedTx.MarshalBinary()
	c.JSON(200, gin.H{"signed_tx": hex.EncodeToString(txBytes)})
}

// AES-GCM 解密工具
func decrypt(ciphertextHex, keyStr string) (string, error) {
	// 1. 将 16 进制字符串转回 byte 数组
	data, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", err
	}

	// 2. 创建 Cipher Block
	// keyStr 长度必须是 16, 24 或 32 (对应 AES-128, AES-192, AES-256)
	block, err := aes.NewCipher([]byte(keyStr))
	if err != nil {
		return "", err
	}

	// 3. 创建 GCM 模式
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// 4. 分离 Nonce (随机数) 和 密文
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]

	// 5. 解密 (Open)
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
