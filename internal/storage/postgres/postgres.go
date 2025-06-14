package postgres

import (
	"adtime-bot/internal/config"
	"adtime-bot/pkg/redis"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

type PostgresStorage struct {
	db     *sqlx.DB
	redis  *redis.Client
	logger *zap.Logger
}

func (s *PostgresStorage) GetUserOrders(ctx context.Context, userID int64) ([]Order, error) {
	const query = `
        SELECT id, width_cm, height_cm, price, status, created_at 
        FROM orders 
        WHERE user_id = $1 AND deleted_at IS NULL
        ORDER BY created_at DESC`

	var orders []Order
	err := s.db.SelectContext(ctx, &orders, query, userID)
	return orders, err
}

func (s *PostgresStorage) DeleteUserData(ctx context.Context, chatID int64) error {
	// Soft delete с timestamp
	_, err := s.db.ExecContext(ctx,
		"UPDATE orders SET deleted_at = NOW() WHERE user_id = $1", chatID)
	return err
}

type Texture struct {
	ID          string  `db:"id"`
	Name        string  `db:"name"`
	PricePerDM2 float64 `db:"price_per_dm2"`
	ImageURL    string  `db:"image_url"`
	InStock     bool    `db:"in_stock"`
}

type Order struct {
	ID          int64     `db:"id"`
	UserID      int64     `db:"user_id"`
	WidthCM     int       `db:"width_cm"`
	HeightCM    int       `db:"height_cm"`
	TextureID   string    `db:"texture_id"`
	TextureName string    `db:"texture_name"`
	Price       float64   `db:"price"`
	LeatherCost float64   `db:"leather_cost"`
	ProcessCost float64   `db:"process_cost"`
	TotalCost   float64   `db:"total_cost"`
	Commission  float64   `db:"commission"`
	Tax         float64   `db:"tax"`
	NetRevenue  float64   `db:"net_revenue"`
	Profit      float64   `db:"profit"`
	Contact     string    `db:"contact"`
	Status      string    `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

type OrderStatistics struct {
	TotalOrders  int
	TotalRevenue float64
	TodayOrders  int
	TodayRevenue float64
	WeekOrders   int
	WeekRevenue  float64
	MonthOrders  int
	MonthRevenue float64
	StatusCounts map[string]int
}

type PriceFormula struct {
	ID          string
	ServiceType string
	Formula     string // "width*height*price*coefficient"
	Parameters  map[string]float64
}

func NewPostgresStorage(ctx context.Context, cfg config.Config, redisClient *redis.Client, logger *zap.Logger) (*PostgresStorage, error) {
	const operation = "storage.NewPostgresStorage"

	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		cfg.Database.Host,
		cfg.Database.Port,
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.Name,
	)

	var db *sqlx.DB
	var err error

	retryPolicy := backoff.NewExponentialBackOff()
	retryPolicy.MaxElapsedTime = 2 * time.Minute
	retryPolicy.MaxInterval = 15 * time.Second

	logger.Info("Connecting to PostgreSQL...")

	err = backoff.RetryNotify(
		func() error {
			db, err = sqlx.ConnectContext(ctx, "postgres", connStr)
			if err != nil {
				return fmt.Errorf("connect: %w", err)
			}

			if err = db.PingContext(ctx); err != nil {
				return fmt.Errorf("ping: %w", err)
			}
			return nil
		},
		retryPolicy,
		func(err error, duration time.Duration) {
			logger.Warn("PostgreSQL connection failed, retrying...",
				zap.Error(err),
				zap.Duration("next_attempt_in", duration))
		},
	)

	if err != nil {
		return nil, fmt.Errorf("%s: failed to connect after retries: %w", operation, err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.Database.ConnMaxIdleTime)

	logger.Info("Successfully connected to PostgreSQL")
	return &PostgresStorage{
		db:     db,
		redis:  redisClient,
		logger: logger,
	}, nil
}

func (s *PostgresStorage) GetTextureByID(ctx context.Context, textureID string) (*Texture, error) {

	cacheKey := fmt.Sprintf("texture:%s", textureID)

	// Try Redis first
	cached, err := s.redis.Get(ctx, cacheKey)
	if err == nil {
		var texture Texture
		if err := json.Unmarshal(cached, &texture); err == nil {
			// Add validation for cached texture
			if texture.PricePerDM2 <= 0 {
				s.logger.Warn("Invalid price in cached texture",
					zap.String("texture_id", textureID),
					zap.Float64("price", texture.PricePerDM2))
				// Force reload from DB by continuing past cache
			} else {
				return &texture, nil
			}
		}
	}

	// Fall back to Postgres
	const query = `
        SELECT id::text, name, price_per_dm2, image_url, in_stock 
        FROM textures 
        WHERE id = $1
    `

	var texture Texture
	err = s.db.GetContext(ctx, &texture, query, textureID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("texture not found: %w", err)
		}
		return nil, fmt.Errorf("failed to get texture: %w", err)
	}

	// Validate price from database
	if texture.PricePerDM2 <= 0 {
		return nil, fmt.Errorf("invalid price for texture %s: %.2f", textureID, texture.PricePerDM2)
	}

	// Cache the validated result
	if data, err := json.Marshal(texture); err == nil {
		s.redis.Set(ctx, cacheKey, data, 24*time.Hour)
	}

	return &texture, nil
}

func (s *PostgresStorage) GetAvailableTextures(ctx context.Context) ([]Texture, error) {
	const query = `SELECT id::text, name, price_per_dm2, image_url FROM textures WHERE in_stock = TRUE`

	var textures []Texture
	err := s.db.SelectContext(ctx, &textures, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get textures: %w", err)
	}

	return textures, nil
}

func (s *PostgresStorage) SaveOrder(ctx context.Context, order Order) (int64, error) {
	const query = `
        INSERT INTO orders (
            user_id, width_cm, height_cm, texture_id, price,
            leather_cost, process_cost, total_cost, commission,
            tax, net_revenue, profit, contact, status, created_at
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
        RETURNING id
    `

	var orderID int64
	err := s.db.QueryRowContext(ctx, query,
		order.UserID,
		order.WidthCM,
		order.HeightCM,
		order.TextureID,
		order.Price,
		order.LeatherCost,
		order.ProcessCost,
		order.TotalCost,
		order.Commission,
		order.Tax,
		order.NetRevenue,
		order.Profit,
		order.Contact,
		order.Status,
		order.CreatedAt,
	).Scan(&orderID)

	if err != nil {
		return 0, fmt.Errorf("failed to save order: %w", err)
	}

	// Invalidate statistics cache
	s.redis.Del(ctx, "order_stats")

	return orderID, nil
}

func (s *PostgresStorage) ExportOrderToExcel(ctx context.Context, order Order) (string, error) {
	f := excelize.NewFile()
	defer f.Close()

	// Create sheet
	index, err := f.NewSheet("Order")
	if err != nil {
		return "", fmt.Errorf("failed to create sheet: %w", err)
	}

	// Set basic order info
	f.SetCellValue("Order", "A1", "Order ID")
	f.SetCellValue("Order", "B1", order.ID)
	f.SetCellValue("Order", "A2", "User ID")
	f.SetCellValue("Order", "B2", order.UserID)
	f.SetCellValue("Order", "A3", "Created At")
	f.SetCellValue("Order", "B3", order.CreatedAt.Format("2006-01-02 15:04"))

	// Set dimensions and calculations
	area := float64(order.WidthCM*order.HeightCM) / 100
	f.SetCellValue("Order", "A4", "Dimensions")
	f.SetCellValue("Order", "B4", fmt.Sprintf("%d × %d cm", order.WidthCM, order.HeightCM))
	f.SetCellValue("Order", "A5", "Area")
	f.SetCellValue("Order", "B5", fmt.Sprintf("%.1f dm²", area))

	// Set pricing info
	f.SetCellValue("Order", "A7", "Price Components")
	f.SetCellValue("Order", "A8", "Leather Cost")
	f.SetCellValue("Order", "B8", order.LeatherCost)
	f.SetCellValue("Order", "A9", "Processing Cost")
	f.SetCellValue("Order", "B9", order.ProcessCost)
	f.SetCellValue("Order", "A10", "Total Cost")
	f.SetCellValue("Order", "B10", order.TotalCost)
	f.SetCellValue("Order", "A11", "Commission")
	f.SetCellValue("Order", "B11", order.Commission)
	f.SetCellValue("Order", "A12", "Tax")
	f.SetCellValue("Order", "B12", order.Tax)
	f.SetCellValue("Order", "A13", "Final Price")
	f.SetCellValue("Order", "B13", order.Price)

	// Formatting
	style, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
	})
	f.SetCellStyle("Order", "A1", "A13", style)

	f.SetActiveSheet(index)

	// Save file
	filename := fmt.Sprintf("order_%d_%s.xlsx",
		order.ID,
		order.CreatedAt.Format("20060102_1504"))
	filepath := fmt.Sprintf("reports/%s", filename)

	if err := os.MkdirAll("reports", 0755); err != nil {
		return "", fmt.Errorf("failed to create reports directory: %w", err)
	}

	if err := f.SaveAs(filepath); err != nil {
		return "", fmt.Errorf("failed to save Excel file: %w", err)
	}

	return filepath, nil
}

func (s *PostgresStorage) ExportAllOrdersToExcel(ctx context.Context, filename string) error {
	const operation = "storage.ExportAllOrdersToExcel"

	// Получаем все заказы из БД
	const query = `
        SELECT o.*, t.name as texture_name 
        FROM orders o
        LEFT JOIN textures t ON o.texture_id = t.id
        ORDER BY o.created_at DESC
    `

	var orders []Order
	if err := s.db.SelectContext(ctx, &orders, query); err != nil {
		s.logger.Error("Failed to fetch orders for export",
			zap.Error(err),
			zap.String("operation", operation))
		return fmt.Errorf("failed to fetch orders: %w", err)
	}

	f := excelize.NewFile()
	defer f.Close()

	index, err := f.NewSheet("Orders")
	if err != nil {
		return fmt.Errorf("failed to create sheet: %w", err)
	}

	// Заголовки
	headers := []string{
		"ID", "User ID", "Width (cm)", "Height (cm)", "Texture ID",
		"Texture Name", "Price", "Leather Cost", "Process Cost",
		"Total Cost", "Commission", "Tax", "Net Revenue", "Profit",
		"Contact", "Status", "Created At",
	}
	for col, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(col+1, 1)
		f.SetCellValue("Orders", cell, header)
	}

	// Данные
	for row, order := range orders {
		data := []interface{}{
			order.ID,
			order.UserID,
			order.WidthCM,
			order.HeightCM,
			order.TextureID,
			order.TextureName,
			order.Price,
			order.LeatherCost,
			order.ProcessCost,
			order.TotalCost,
			order.Commission,
			order.Tax,
			order.NetRevenue,
			order.Profit,
			order.Contact,
			order.Status,
			order.CreatedAt.Format("2006-01-02 15:04"),
		}
		for col, value := range data {
			cell, _ := excelize.CoordinatesToCellName(col+1, row+2)
			f.SetCellValue("Orders", cell, value)
		}
	}

	f.SetActiveSheet(index)

	// Создаем папку если не существует
	if err := os.MkdirAll("reports", 0755); err != nil {
		return fmt.Errorf("failed to create reports directory: %w", err)
	}

	// Сохраняем в один файл
	filepath := fmt.Sprintf("reports/%s.xlsx", filename)
	if err := f.SaveAs(filepath); err != nil {
		return fmt.Errorf("failed to save Excel file: %w", err)
	}

	return nil
}

func (s *PostgresStorage) SaveUserAgreement(ctx context.Context, userID int64, phone string) error {
	const query = `
        INSERT INTO users (user_id, agreed_to_tpa, phone_number)
        VALUES ($1, TRUE, $2)
        ON CONFLICT (user_id) 
        DO UPDATE SET agreed_to_tpa = TRUE, phone_number = $2
    `
	_, err := s.db.ExecContext(ctx, query, userID, phone)
	return err
}

func (s *PostgresStorage) GetUserAgreement(ctx context.Context, userID int64) (bool, string, error) {
	const query = `
		SELECT agreed_to_tpa, phone_number 
		FROM users 
		WHERE user_id = $1
	`

	var agreed bool
	var phone string
	err := s.db.QueryRowContext(ctx, query, userID).Scan(&agreed, &phone)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return agreed, phone, err
}

func (s *PostgresStorage) UpdateOrderStatus(ctx context.Context, orderID int64, status string) error {
	// Get all orders
	const query = `
		SELECT * 
		FROM orders 
		ORDER BY created_at 
		DESC
	`

	var orders []Order
	if err := s.db.SelectContext(ctx, &orders, query); err != nil {
		return fmt.Errorf("failed to fetch orders: %w", err)
	}

	// Create or open file
	filename := "reports/current_orders.xlsx"
	f := excelize.NewFile()

	if _, err := os.Stat(filename); err == nil {
		f, err = excelize.OpenFile(filename)
		if err != nil {
			return fmt.Errorf("failed to open existing file: %w", err)
		}
		// Clear existing data if needed
		if err := f.DeleteSheet("Orders"); err != nil {
			return fmt.Errorf("failed to clear old sheet: %w", err)
		}
	}

	// Create fresh sheet
	index, err := f.NewSheet("Orders")
	if err != nil {
		return fmt.Errorf("failed to create sheet: %w", err)
	}

	// Заголовки
	headers := []string{
		"ID", "User ID", "Width (cm)", "Height (cm)", "Texture ID",
		"Texture Name", "Price", "Leather Cost", "Process Cost",
		"Total Cost", "Commission", "Tax", "Net Revenue", "Profit",
		"Contact", "Status", "Created At",
	}
	for col, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(col+1, 1)
		f.SetCellValue("Orders", cell, header)
	}

	// Данные
	for row, order := range orders {
		data := []interface{}{
			order.ID,
			order.UserID,
			order.WidthCM,
			order.HeightCM,
			order.TextureID,
			order.TextureName,
			order.Price,
			order.LeatherCost,
			order.ProcessCost,
			order.TotalCost,
			order.Commission,
			order.Tax,
			order.NetRevenue,
			order.Profit,
			order.Contact,
			order.Status,
			order.CreatedAt.Format("2006-01-02 15:04"),
		}
		for col, value := range data {
			cell, _ := excelize.CoordinatesToCellName(col+1, row+2)
			f.SetCellValue("Orders", cell, value)
		}
	}

	f.SetActiveSheet(index)

	// Создаем папку если не существует
	if err := os.MkdirAll("reports", 0755); err != nil {
		return fmt.Errorf("failed to create reports directory: %w", err)
	}

	// Сохраняем в один файл
	filepath := fmt.Sprintf("reports/%s.xlsx", filename)
	if err := f.SaveAs(filepath); err != nil {
		return fmt.Errorf("failed to save Excel file: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll("reports", 0755); err != nil {
		return fmt.Errorf("failed to create reports directory: %w", err)
	}

	return f.SaveAs(filename)
}

func (s *PostgresStorage) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *PostgresStorage) GetOrderByID(ctx context.Context, orderID int64) (*Order, error) {
	const query = `SELECT * FROM orders WHERE id = $1`
	var order Order
	err := s.db.GetContext(ctx, &order, query, orderID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("order not found")
		}
		return nil, fmt.Errorf("failed to get order: %w", err)
	}
	return &order, nil
}

func (s *PostgresStorage) GetOrderStatistics(ctx context.Context) (*OrderStatistics, error) {
	cacheKey := "order_stats"

	// Try Redis first
	if cached, err := s.redis.Get(ctx, cacheKey); err == nil {
		var stats OrderStatistics
		if err := json.Unmarshal(cached, &stats); err == nil {
			return &stats, nil
		}
	}

	stats := &OrderStatistics{
		StatusCounts: make(map[string]int),
	}

	// Get total orders and revenue
	err := s.db.QueryRowContext(ctx, `
        SELECT 
            COUNT(*) as total_orders,
            COALESCE(SUM(price), 0) as total_revenue
        FROM orders
    `).Scan(&stats.TotalOrders, &stats.TotalRevenue)
	if err != nil {
		return nil, fmt.Errorf("failed to get total stats: %w", err)
	}

	// Get today's stats
	err = s.db.QueryRowContext(ctx, `
        SELECT 
            COUNT(*) as count,
            COALESCE(SUM(price), 0) as revenue
        FROM orders
        WHERE created_at >= CURRENT_DATE
    `).Scan(&stats.TodayOrders, &stats.TodayRevenue)
	if err != nil {
		return nil, fmt.Errorf("failed to get today's stats: %w", err)
	}

	// Get week's stats
	err = s.db.QueryRowContext(ctx, `
        SELECT 
            COUNT(*) as count,
            COALESCE(SUM(price), 0) as revenue
        FROM orders
        WHERE created_at >= CURRENT_DATE - INTERVAL '7 days'
    `).Scan(&stats.WeekOrders, &stats.WeekRevenue)
	if err != nil {
		return nil, fmt.Errorf("failed to get week's stats: %w", err)
	}

	// Get month's stats
	err = s.db.QueryRowContext(ctx, `
        SELECT 
            COUNT(*) as count,
            COALESCE(SUM(price), 0) as revenue
        FROM orders
        WHERE created_at >= CURRENT_DATE - INTERVAL '30 days'
    `).Scan(&stats.MonthOrders, &stats.MonthRevenue)
	if err != nil {
		return nil, fmt.Errorf("failed to get month's stats: %w", err)
	}

	// Get status counts - fixed version
	type statusCount struct {
		Status string `db:"status"`
		Count  int    `db:"count"`
	}

	var statusCounts []statusCount
	err = s.db.SelectContext(ctx, &statusCounts, `
        SELECT status, COUNT(*) as count
        FROM orders
        GROUP BY status
    `)
	if err != nil {
		return nil, fmt.Errorf("failed to get status counts: %w", err)
	}

	for _, sc := range statusCounts {
		stats.StatusCounts[sc.Status] = sc.Count
	}

	// Cache the result
	if data, err := json.Marshal(stats); err == nil {
		s.redis.Set(ctx, cacheKey, data, 1*time.Hour)
	}

	return stats, nil
}

func (s *PostgresStorage) CheckRateLimit(ctx context.Context, userID int64, action string, limit int64, window time.Duration) (bool, error) {
	key := fmt.Sprintf("ratelimit:%d:%s", userID, action)

	count, err := s.redis.Incr(ctx, key)
	if err != nil {
		return false, fmt.Errorf("failed to increment rate limit counter: %w", err)
	}

	// Set expiry if this is the first increment
	if count == 1 {
		if _, err := s.redis.Expire(ctx, key, window); err != nil {
			return false, fmt.Errorf("failed to set rate limit window: %w", err)
		}
	}

	return count > limit, nil
}

func (s *PostgresStorage) GetTextureByName(ctx context.Context, name string) (*Texture, error) {
	const query = `SELECT id::text, name, price_per_dm2 FROM textures WHERE name = $1`

	var texture Texture
	err := s.db.GetContext(ctx, &texture, query, name)
	if err != nil {
		return nil, fmt.Errorf("failed to get texture: %w", err)
	}

	return &texture, nil
}
