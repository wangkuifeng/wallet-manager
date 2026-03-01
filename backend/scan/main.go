package scan

import (
	"context"
	"database/sql"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	_ "github.com/go-sql-driver/mysql"
)

// --- 配置 ---
const (
	RPC_URL       = "https://mainnet.infura.io/v3/YOUR_KEY" // 或本地节点
	DSN           = "root:123456@tcp(127.0.0.1:3306)/business_wallet?charset=utf8mb4&parseTime=True"
	CONFIRMATIONS = 6 // 延迟6个块，防止分叉
	SCAN_INTERVAL = 3 * time.Second
)

var db *sql.DB
var client *ethclient.Client

// 内存地址缓存 map[address]user_id
var addressCache = make(map[string]string)

func main() {
	var err error

	// 1. 初始化数据库
	db, err = sql.Open("mysql", DSN)
	if err != nil {
		log.Fatal(err)
	}

	// 2. 连接以太坊节点
	client, err = ethclient.Dial(RPC_URL)
	if err != nil {
		log.Fatal(err)
	}

	// 3. 预加载所有用户地址到内存 (启动时载入一次，生产环境需定期刷新)
	loadAddressCache()

	log.Println("扫描服务启动...")

	// 4. 开始循环扫描
	for {
		scanLoop()
		time.Sleep(SCAN_INTERVAL)
	}
}

// 地址缓存加载逻辑
func loadAddressCache() {
	rows, err := db.Query("SELECT user_id, address FROM user_accounts")
	if err != nil {
		log.Println("加载地址缓存失败:", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var uid, addr string
		if err := rows.Scan(&uid, &addr); err == nil {
			// 以太坊地址不区分大小写，但建议统一转小写存储和比对
			addressCache[strings.ToLower(addr)] = uid
			count++
		}
	}
	log.Printf("已加载 %d 个用户地址到内存", count)
}

// 核心扫描循环
func scanLoop() {
	// 1. 获取链上最新高度
	header, err := client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Println("获取最新区块头失败:", err)
		return
	}
	latestBlock := header.Number.Uint64()

	// 2. 获取数据库里我们扫到的位置
	var currentBlock uint64
	err = db.QueryRow("SELECT current_block FROM scan_cursor WHERE chain_type='ETH'").Scan(&currentBlock)
	if err != nil {
		log.Println("读取游标失败:", err)
		return
	}
	//增加
	// 3. 计算目标区块 (当前进度 + 1)
	targetBlock := currentBlock + 1

	// 如果目标区块过于接近最新块 (不满足安全确认数)，则等待
	if targetBlock > latestBlock-CONFIRMATIONS {
		return // 等待出块
	}

	// 4. 处理区块
	processBlock(targetBlock)
}

// 区块与交易处理
func processBlock(blockNum uint64) {
	blockBig := new(big.Int).SetUint64(blockNum)

	// 获取完整区块 (包含交易详情)
	block, err := client.BlockByNumber(context.Background(), blockBig)
	if err != nil {
		log.Printf("下载区块 %d 失败: %v", blockNum, err)
		return
	}

	log.Printf("正在扫描区块: %d, 包含交易数: %d", blockNum, len(block.Transactions()))

	// 开启数据库事务 (确保 数据插入 和 游标更新 是原子操作)
	txDB, err := db.Begin()
	if err != nil {
		return
	}

	// 遇到错误回滚
	defer txDB.Rollback()

	for _, tx := range block.Transactions() {
		// 排除合约创建交易 (To == nil)
		if tx.To() == nil {
			continue
		}

		// 检查 To 地址是否在我们的缓存中
		toAddr := strings.ToLower(tx.To().Hex())
		userID, exists := addressCache[toAddr]

		if exists {
			// --- 命中！发现充值 ---
			log.Printf(">>>>> 发现充值! 用户: %s, 金额: %s Wei", userID, tx.Value().String())

			// 转换为 ETH 单位 (简单除以 1e18，生产环境建议用 big.Float)
			amountWei := new(big.Float).SetInt(tx.Value())
			amountEth := new(big.Float).Quo(amountWei, big.NewFloat(1e18))
			amountStr := amountEth.Text('f', 18)

			// 插入充值记录 (使用 IGNORE 或 判断 exists 防止重复)
			_, err := txDB.Exec(`
				INSERT INTO user_deposits (tx_hash, user_id, address, amount, block_number)
				VALUES (?, ?, ?, ?, ?)`,
				tx.Hash().Hex(), userID, toAddr, amountStr, blockNum,
			)
			if err != nil {
				// 如果是主键冲突(重复扫描)，可以忽略，否则打印错误
				log.Println("插入充值记录警告:", err)
			}
		}
	}

	// 5. 更新游标 (这一步非常重要！)
	_, err = txDB.Exec("UPDATE scan_cursor SET current_block = ? WHERE chain_type='ETH'", blockNum)
	if err != nil {
		log.Println("更新游标失败:", err)
		return
	}

	// 提交事务
	if err := txDB.Commit(); err != nil {
		log.Println("事务提交失败:", err)
	} else {
		log.Printf("区块 %d 处理完成", blockNum)
	}
}
