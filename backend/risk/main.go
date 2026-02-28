package risk

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	_ "github.com/go-sql-driver/mysql"
)

// --- 配置 ---
const (
	RPC_URL          = "https://mainnet.infura.io/v3/YOUR_KEY"
	DSN              = "root:123456@tcp(127.0.0.1:3306)/business_wallet?charset=utf8mb4&parseTime=True"
	REQUIRED_CONFIRM = 12    // 风控要求：必须等待 12 个区块确认才入账
	LARGE_AMOUNT     = 100.0 // 风控阈值：超过 100 ETH 需要人工审核
)

// 数据模型
type DepositRecord struct {
	ID          int64
	TxHash      string
	UserID      string
	FromAddress string
	Amount      float64 // 简化演示，生产环境请用 big.Int / decimal
	BlockNumber uint64
	Status      int
}

var db *sql.DB
var client *ethclient.Client

func main() {
	var err error
	db, err = sql.Open("mysql", DSN)
	if err != nil {
		log.Fatal(err)
	}

	client, err = ethclient.Dial(RPC_URL)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("风控服务启动 (Risk Control)...")

	// 轮询处理
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		processPendingDeposits()
	}
}

// 核心处理逻辑 (Process Loop)
func processPendingDeposits() {
	// 1. 获取当前链上高度 (用于计算确认数)
	header, err := client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Println("获取链上高度失败:", err)
		return
	}
	currentHeight := header.Number.Uint64()

	// 2. 开启事务
	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback() // 遇到错误自动回滚

	// 3. 查询待处理记录 (Status = 0)
	// 注意：使用 FOR UPDATE 锁住这几行，防止其他风控实例争抢
	rows, err := tx.Query(`
		SELECT id, tx_hash, user_id, from_address, amount, block_number 
		FROM user_deposits 
		WHERE status = 0 
		LIMIT 10 FOR UPDATE`)
	if err != nil {
		return
	}
	defer rows.Close()

	var deposits []DepositRecord
	for rows.Next() {
		var d DepositRecord
		if err := rows.Scan(&d.ID, &d.TxHash, &d.UserID, &d.FromAddress, &d.Amount, &d.BlockNumber); err == nil {
			deposits = append(deposits, d)
		}
	}

	// 4. 逐条执行风控规则
	for _, deposit := range deposits {
		checkAndSettle(tx, deposit, currentHeight)
	}

	// 5. 提交事务
	tx.Commit()
}

// 风控规则引擎 (Rule Engine)
func checkAndSettle(tx *sql.Tx, d DepositRecord, currentHeight uint64) {
	// --- 规则 1: 确认数检查 (Anti-Reorg) ---
	// 确认数 = 当前高度 - 充值交易所在高度
	confirmations := currentHeight - d.BlockNumber
	if confirmations < REQUIRED_CONFIRM {
		// 还没到安全确认数，跳过，等下一次轮询再看
		// log.Printf("交易 %s 等待确认中 (%d/%d)", d.TxHash, confirmations, REQUIRED_CONFIRM)
		return
	}

	// --- 规则 2: 黑名单检查 (AML - Anti Money Laundering) ---
	// 检查资金来源地址是否在黑名单中
	var blacklisted int
	err := tx.QueryRow("SELECT 1 FROM address_blacklist WHERE address = ?", d.FromAddress).Scan(&blacklisted)
	if err == nil {
		// 命中黑名单！
		log.Printf("!!! 风险拦截 !!! 用户 %s 收到来自黑名单地址 %s 的资金", d.UserID, d.FromAddress)

		// 标记为 Status=2 (冻结)，不加余额
		tx.Exec("UPDATE user_deposits SET status = 2 WHERE id = ?", d.ID)
		return
	}

	// --- 规则 3: 大额预警 (Large Amount) ---
	if d.Amount > LARGE_AMOUNT {
		log.Printf("!!! 大额预警 !!! 用户 %s 充值 %f ETH，转入人工审核", d.UserID, d.Amount)
		// 标记为 Status=3 (待人工复核)
		tx.Exec("UPDATE user_deposits SET status = 3 WHERE id = ?", d.ID)
		return
	}

	// ===========================
	// === 风控通过，执行入账 ===
	// ===========================

	// A. 给用户加钱
	// 这里假设 user_accounts 表有 balance 字段
	// 如果是第一次入账，注意处理并发 (SQL层面的原子更新)
	res, err := tx.Exec("UPDATE user_accounts SET balance = balance + ? WHERE user_id = ?", d.Amount, d.UserID)
	if err != nil {
		log.Println("更新余额失败:", err)
		return
	}

	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		log.Printf("用户 %s 不存在，入账失败", d.UserID)
		// 可以在这里创建用户，或者记录错误日志
		return
	}

	// B. 更新订单状态为成功 (Status=1)
	_, err = tx.Exec("UPDATE user_deposits SET status = 1 WHERE id = ?", d.ID)
	if err != nil {
		log.Println("更新订单状态失败:", err)
		return
	}

	log.Printf("SUCCESS: 用户 %s 入账 %f ETH (Tx: %s)", d.UserID, d.Amount, d.TxHash)
}
