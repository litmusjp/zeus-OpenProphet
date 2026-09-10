package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"prophet-trader/database"
	"prophet-trader/interfaces"
	"prophet-trader/models"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

func newClientOrderID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}

	return "op-" + hex.EncodeToString(bytes), nil
}

// ManagedPosition represents a position with automated risk management
type ManagedPosition struct {
	ID       string `json:"id"`
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`     // "buy" or "sell"
	Strategy string `json:"strategy"` // "SWING_TRADE", "LONG_TERM", "DAY_TRADE"

	// Entry details
	Quantity          float64 `json:"quantity"`
	EntryPrice        float64 `json:"entry_price"`
	EntryOrderID      string  `json:"entry_order_id"`
	EntryOrderType    string  `json:"entry_order_type"` // "market", "limit"
	AllocationDollars float64 `json:"allocation_dollars"`

	// Risk management
	StopLossPrice   float64 `json:"stop_loss_price"`
	StopLossPercent float64 `json:"stop_loss_percent"`
	StopLossOrderID string  `json:"stop_loss_order_id,omitempty"`
	TrailingStop    bool    `json:"trailing_stop"`
	TrailingPercent float64 `json:"trailing_percent,omitempty"`

	// Profit targets
	TakeProfitPrice         float64 `json:"take_profit_price"`
	TakeProfitPercent       float64 `json:"take_profit_percent"`
	TakeProfitOrderID       string  `json:"take_profit_order_id,omitempty"`
	CloseOrderID            string  `json:"close_order_id,omitempty"`
	CloseOrderClientOrderID string  `json:"close_order_client_order_id,omitempty"`

	// Partial exit strategy
	PartialExit       *PartialExitConfig `json:"partial_exit,omitempty"`
	PartialExitOrders []string           `json:"partial_exit_orders,omitempty"`

	// Status tracking
	Status             string             `json:"status"` // "PENDING", "ACTIVE", "PARTIAL", "CLOSING", "CLOSED", "STOPPED_OUT", "FAILED"
	CurrentPrice       float64            `json:"current_price"`
	UnrealizedPL       float64            `json:"unrealized_pl"`
	UnrealizedPLPC     float64            `json:"unrealized_pl_percent"`
	RemainingQty       float64            `json:"remaining_qty"`
	ProcessedExitFills map[string]float64 `json:"processed_exit_fills,omitempty"`

	// Metadata
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	Notes     string     `json:"notes,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
}

// PartialExitConfig defines partial profit taking strategy
type PartialExitConfig struct {
	Enabled       bool    `json:"enabled"`
	Percent       float64 `json:"percent"`        // % of position to exit
	TargetPercent float64 `json:"target_percent"` // % gain to trigger partial exit
	TargetPrice   float64 `json:"target_price"`   // Calculated target price
}

// PlaceManagedPositionRequest represents request to open a managed position
type PlaceManagedPositionRequest struct {
	Symbol            string  `json:"symbol" binding:"required"`
	Side              string  `json:"side" binding:"required"` // "buy" or "sell"
	Strategy          string  `json:"strategy"`                // "SWING_TRADE", "LONG_TERM", "DAY_TRADE"
	AllocationDollars float64 `json:"allocation_dollars" binding:"required,gt=0"`

	// Entry configuration
	EntryStrategy string   `json:"entry_strategy"`        // "market", "limit"
	EntryPrice    *float64 `json:"entry_price,omitempty"` // Required for limit orders

	// Risk management (one of these required)
	StopLossPrice   *float64 `json:"stop_loss_price,omitempty"`
	StopLossPercent *float64 `json:"stop_loss_percent,omitempty"`
	TrailingStop    bool     `json:"trailing_stop"`
	TrailingPercent float64  `json:"trailing_percent,omitempty"`

	// Profit targets (one of these required)
	TakeProfitPrice   *float64 `json:"take_profit_price,omitempty"`
	TakeProfitPercent *float64 `json:"take_profit_percent,omitempty"`

	// Partial exit (optional)
	PartialExit *PartialExitConfig `json:"partial_exit,omitempty"`

	// Metadata
	Notes string   `json:"notes,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

// PositionManager handles automated position management
type PositionManager struct {
	tradingService interfaces.TradingService
	dataService    interfaces.DataService
	storageService *database.LocalStorage

	positions map[string]*ManagedPosition // position_id -> position
	mu        sync.RWMutex
	closeMu   sync.Mutex
	logger    *logrus.Logger

	ctx    context.Context
	cancel context.CancelFunc
}

// NewPositionManager creates a new position manager
func NewPositionManager(
	tradingService interfaces.TradingService,
	dataService interfaces.DataService,
	storageService *database.LocalStorage,
) *PositionManager {
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	ctx, cancel := context.WithCancel(context.Background())

	pm := &PositionManager{
		tradingService: tradingService,
		dataService:    dataService,
		storageService: storageService,
		positions:      make(map[string]*ManagedPosition),
		logger:         logger,
		ctx:            ctx,
		cancel:         cancel,
	}

	// Load existing positions from database
	if err := pm.loadPositionsFromDB(); err != nil {
		logger.WithError(err).Error("Failed to load positions from database")
	}

	return pm
}

// PlaceManagedPosition opens a new managed position with automated risk management
func (pm *PositionManager) PlaceManagedPosition(ctx context.Context, req *PlaceManagedPositionRequest) (*ManagedPosition, error) {
	pm.logger.WithFields(logrus.Fields{
		"symbol":     req.Symbol,
		"side":       req.Side,
		"allocation": req.AllocationDollars,
	}).Info("Placing managed position")

	// Validate request
	if err := pm.validateRequest(req); err != nil {
		return nil, fmt.Errorf("invalid request: %w", err)
	}

	// Get current price for calculations
	currentPrice, err := pm.getCurrentPrice(ctx, req.Symbol)
	if err != nil {
		return nil, fmt.Errorf("failed to get current price: %w", err)
	}

	// Calculate position parameters
	entryPrice := currentPrice
	if req.EntryPrice != nil {
		entryPrice = *req.EntryPrice
	}

	quantity := pm.calculateQuantity(req.AllocationDollars, entryPrice)

	// Calculate stop loss
	stopLossPrice := pm.calculateStopLoss(entryPrice, req.StopLossPrice, req.StopLossPercent, req.Side)
	stopLossPercent := math.Abs((stopLossPrice - entryPrice) / entryPrice * 100)

	// Calculate take profit
	takeProfitPrice := pm.calculateTakeProfit(entryPrice, req.TakeProfitPrice, req.TakeProfitPercent, req.Side)
	takeProfitPercent := math.Abs((takeProfitPrice - entryPrice) / entryPrice * 100)

	// Calculate partial exit if configured
	if req.PartialExit != nil && req.PartialExit.Enabled {
		req.PartialExit.TargetPrice = pm.calculatePartialExitPrice(entryPrice, req.PartialExit.TargetPercent, req.Side)
	}

	// Create managed position
	position := &ManagedPosition{
		ID:                pm.generatePositionID(),
		Symbol:            req.Symbol,
		Side:              req.Side,
		Strategy:          req.Strategy,
		Quantity:          quantity,
		EntryPrice:        entryPrice,
		EntryOrderType:    req.EntryStrategy,
		AllocationDollars: req.AllocationDollars,
		StopLossPrice:     stopLossPrice,
		StopLossPercent:   stopLossPercent,
		TrailingStop:      req.TrailingStop,
		TrailingPercent:   req.TrailingPercent,
		TakeProfitPrice:   takeProfitPrice,
		TakeProfitPercent: takeProfitPercent,
		PartialExit:       req.PartialExit,
		Status:            "PENDING",
		CurrentPrice:      currentPrice,
		RemainingQty:      quantity,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
		Notes:             req.Notes,
		Tags:              req.Tags,
	}

	// Place entry order
	if err := pm.placeEntryOrder(ctx, position); err != nil {
		return nil, fmt.Errorf("failed to place entry order: %w", err)
	}

	// Store position
	pm.mu.Lock()
	pm.positions[position.ID] = position
	pm.mu.Unlock()

	// Save to database
	if err := pm.savePositionToDB(position); err != nil {
		pm.logger.WithError(err).Error("Failed to save position to database")
	}

	pm.logger.WithFields(logrus.Fields{
		"position_id":       position.ID,
		"entry_order_id":    position.EntryOrderID,
		"quantity":          quantity,
		"entry_price":       entryPrice,
		"stop_loss":         stopLossPrice,
		"take_profit":       takeProfitPrice,
		"risk_reward_ratio": takeProfitPercent / stopLossPercent,
	}).Info("Managed position created")

	return position, nil
}

// placeEntryOrder places the initial entry order
func (pm *PositionManager) placeEntryOrder(ctx context.Context, position *ManagedPosition) error {
	orderType := "market"
	if position.EntryOrderType == "limit" {
		orderType = "limit"
	}

	clientOrderID, err := newClientOrderID()
	if err != nil {
		pm.logger.WithError(err).Warn("Failed to generate client order id; placing order without one")
		clientOrderID = ""
	}

	order := &interfaces.Order{
		ClientOrderID: clientOrderID,
		Symbol:        position.Symbol,
		Qty:           position.Quantity,
		Side:          position.Side,
		Type:          orderType,
		TimeInForce:   "gtc",
		Status:        "pending",
		SubmittedAt:   time.Now(),
	}

	if orderType == "limit" {
		order.LimitPrice = &position.EntryPrice
	}

	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Error("Failed to persist managed position entry order intent")
		return err
	}

	result, err := pm.tradingService.PlaceOrder(ctx, order)
	if err != nil {
		order.Status = "submit_failed"
		if saveErr := pm.storageService.SaveOrder(order); saveErr != nil {
			pm.logger.WithError(saveErr).Warn("Failed to record managed position entry submission failure")
		}
		return err
	}

	order.ID = result.OrderID
	order.Status = result.Status
	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Warn("Failed to update managed position entry order after submission")
	}

	position.EntryOrderID = result.OrderID
	position.Status = "PENDING"

	return nil
}

// MonitorPositions monitors all active positions and manages risk
func (pm *PositionManager) MonitorPositions(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second) // Check every 10 seconds
	defer ticker.Stop()

	pm.logger.Info("Position monitoring started")

	for {
		select {
		case <-ctx.Done():
			pm.logger.Info("Position monitoring stopped")
			return
		case <-ticker.C:
			pm.checkPositions(ctx)
		}
	}
}

// checkPositions checks all positions and manages their risk orders
func (pm *PositionManager) checkPositions(ctx context.Context) {
	pm.mu.RLock()
	positions := make([]*ManagedPosition, 0, len(pm.positions))
	for _, pos := range pm.positions {
		positions = append(positions, pos)
	}
	pm.mu.RUnlock()

	for _, position := range positions {
		pm.closeMu.Lock()
		func() {
			defer pm.closeMu.Unlock()
			if position.Status == "CLOSED" || position.Status == "STOPPED_OUT" {
				return
			}

			// Check if entry order filled
			if position.Status == "PENDING" {
				pm.checkEntryOrder(ctx, position)
				return
			}

			// Update current price and P&L
			if err := pm.updatePositionPrice(ctx, position); err != nil {
				pm.logger.WithError(err).WithField("symbol", position.Symbol).Error("Failed to update position price")
				return
			}

			// Check if we need to place/update risk orders
			if position.Status == "ACTIVE" || position.Status == "PARTIAL" || position.Status == "CLOSING" {
				pm.manageRiskOrders(ctx, position)
			}

			// Check trailing stop
			if position.TrailingStop {
				pm.updateTrailingStop(ctx, position)
			}
		}()
	}
}

// checkEntryOrder checks if entry order has filled
func (pm *PositionManager) checkEntryOrder(ctx context.Context, position *ManagedPosition) {
	order, err := pm.tradingService.GetOrder(ctx, position.EntryOrderID)
	if err != nil {
		pm.logger.WithError(err).Error("Failed to get entry order")
		return
	}

	if order.Status == "filled" || (order.Status == "partially_filled" && order.FilledQty > 0) {
		if order.FilledAvgPrice != nil {
			position.EntryPrice = *order.FilledAvgPrice
		}
		if order.FilledQty > 0 {
			position.RemainingQty = math.Min(position.Quantity, order.FilledQty)
		}
		position.UpdatedAt = time.Now()

		if order.Status == "filled" {
			position.Status = "ACTIVE"
		}
		pm.placeRiskOrders(ctx, position)
		if order.Status == "filled" {
			if err := pm.resizeProtection(ctx, position); err != nil {
				pm.logger.WithError(err).Error("Failed to resize protection after entry completion")
			}
		}

		pm.logger.WithFields(logrus.Fields{
			"position_id": position.ID,
			"symbol":      position.Symbol,
			"fill_price":  position.EntryPrice,
			"filled_qty":  position.RemainingQty,
		}).Info("Entry order fill reconciled")

		// Save to database
		pm.savePositionToDB(position)
	}
}

// placeRiskOrders places stop loss and take profit orders
func (pm *PositionManager) placeRiskOrders(ctx context.Context, position *ManagedPosition) {
	// Place stop loss order
	if err := pm.placeStopLossOrder(ctx, position); err != nil {
		pm.logger.WithError(err).Error("Failed to place stop loss order")
	}

	// Place take profit order
	if err := pm.placeTakeProfitOrder(ctx, position); err != nil {
		pm.logger.WithError(err).Error("Failed to place take profit order")
	}

	// Place partial exit order if configured
	if position.PartialExit != nil && position.PartialExit.Enabled {
		if err := pm.placePartialExitOrder(ctx, position); err != nil {
			pm.logger.WithError(err).Error("Failed to place partial exit order")
		}
	}
}

// placeStopLossOrder places or updates stop loss order
func (pm *PositionManager) placeStopLossOrder(ctx context.Context, position *ManagedPosition) error {
	if position.StopLossOrderID != "" {
		return nil
	}
	exitSide := "sell"
	if position.Side == "sell" {
		exitSide = "buy"
	}

	clientOrderID, err := newClientOrderID()
	if err != nil {
		pm.logger.WithError(err).Warn("Failed to generate client order id; placing order without one")
		clientOrderID = ""
	}

	order := &interfaces.Order{
		ClientOrderID: clientOrderID,
		Symbol:        position.Symbol,
		Qty:           position.RemainingQty,
		Side:          exitSide,
		Type:          "stop",
		TimeInForce:   "gtc",
		StopPrice:     &position.StopLossPrice,
		Status:        "pending",
		SubmittedAt:   time.Now(),
	}

	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Warn("Failed to persist stop loss order intent before submit")
	}

	result, err := pm.tradingService.PlaceOrder(ctx, order)
	if err != nil {
		order.Status = "submit_failed"
		if saveErr := pm.storageService.SaveOrder(order); saveErr != nil {
			pm.logger.WithError(saveErr).Warn("Failed to record stop loss submission failure")
		}
		return err
	}

	order.ID = result.OrderID
	order.Status = result.Status
	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Warn("Failed to save stop loss order")
	}

	position.StopLossOrderID = result.OrderID
	pm.logger.WithFields(logrus.Fields{
		"position_id": position.ID,
		"order_id":    result.OrderID,
		"stop_price":  position.StopLossPrice,
	}).Info("Stop loss order placed")

	return nil
}

// placeTakeProfitOrder places take profit limit order
func (pm *PositionManager) placeTakeProfitOrder(ctx context.Context, position *ManagedPosition) error {
	if position.TakeProfitOrderID != "" {
		return nil
	}
	exitSide := "sell"
	if position.Side == "sell" {
		exitSide = "buy"
	}

	clientOrderID, err := newClientOrderID()
	if err != nil {
		pm.logger.WithError(err).Warn("Failed to generate client order id; placing order without one")
		clientOrderID = ""
	}

	order := &interfaces.Order{
		ClientOrderID: clientOrderID,
		Symbol:        position.Symbol,
		Qty:           position.RemainingQty,
		Side:          exitSide,
		Type:          "limit",
		TimeInForce:   "gtc",
		LimitPrice:    &position.TakeProfitPrice,
		Status:        "pending",
		SubmittedAt:   time.Now(),
	}

	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Warn("Failed to persist take profit order intent before submit")
	}

	result, err := pm.tradingService.PlaceOrder(ctx, order)
	if err != nil {
		order.Status = "submit_failed"
		if saveErr := pm.storageService.SaveOrder(order); saveErr != nil {
			pm.logger.WithError(saveErr).Warn("Failed to record take profit submission failure")
		}
		return err
	}

	order.ID = result.OrderID
	order.Status = result.Status
	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Warn("Failed to save take profit order")
	}

	position.TakeProfitOrderID = result.OrderID
	pm.logger.WithFields(logrus.Fields{
		"position_id": position.ID,
		"order_id":    result.OrderID,
		"limit_price": position.TakeProfitPrice,
	}).Info("Take profit order placed")

	return nil
}

// placePartialExitOrder places partial exit order
func (pm *PositionManager) placePartialExitOrder(ctx context.Context, position *ManagedPosition) error {
	if len(position.PartialExitOrders) > 0 {
		return nil
	}
	exitSide := "sell"
	if position.Side == "sell" {
		exitSide = "buy"
	}

	partialQty := position.RemainingQty * (position.PartialExit.Percent / 100.0)

	clientOrderID, err := newClientOrderID()
	if err != nil {
		pm.logger.WithError(err).Warn("Failed to generate client order id; placing order without one")
		clientOrderID = ""
	}

	order := &interfaces.Order{
		ClientOrderID: clientOrderID,
		Symbol:        position.Symbol,
		Qty:           partialQty,
		Side:          exitSide,
		Type:          "limit",
		TimeInForce:   "gtc",
		LimitPrice:    &position.PartialExit.TargetPrice,
		Status:        "pending",
		SubmittedAt:   time.Now(),
	}

	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Warn("Failed to persist partial exit order intent before submit")
	}

	result, err := pm.tradingService.PlaceOrder(ctx, order)
	if err != nil {
		order.Status = "submit_failed"
		if saveErr := pm.storageService.SaveOrder(order); saveErr != nil {
			pm.logger.WithError(saveErr).Warn("Failed to record partial exit submission failure")
		}
		return err
	}

	order.ID = result.OrderID
	order.Status = result.Status
	if err := pm.storageService.SaveOrder(order); err != nil {
		pm.logger.WithError(err).Warn("Failed to save partial exit order")
	}

	position.PartialExitOrders = append(position.PartialExitOrders, result.OrderID)
	pm.logger.WithFields(logrus.Fields{
		"position_id": position.ID,
		"order_id":    result.OrderID,
		"quantity":    partialQty,
		"limit_price": position.PartialExit.TargetPrice,
	}).Info("Partial exit order placed")

	return nil
}

func isTerminalOrderStatus(status string) bool {
	switch status {
	case "filled", "canceled", "cancelled", "rejected", "expired", "done", "submit_failed":
		return true
	default:
		return false
	}
}

func (pm *PositionManager) cancelAndConfirm(ctx context.Context, orderID string) error {
	if orderID == "" {
		return nil
	}
	order, err := pm.tradingService.GetOrder(ctx, orderID)
	if err != nil {
		return fmt.Errorf("failed to reconcile order %s before cancellation: %w", orderID, err)
	}
	if order == nil {
		return fmt.Errorf("broker returned no order for %s", orderID)
	}
	if isTerminalOrderStatus(order.Status) {
		if order.Status == "filled" {
			return fmt.Errorf("order %s filled while it was being closed", orderID)
		}
		return nil
	}
	if err := pm.tradingService.CancelOrder(ctx, orderID); err != nil {
		return fmt.Errorf("failed to cancel order %s: %w", orderID, err)
	}
	confirmed, err := pm.tradingService.GetOrder(ctx, orderID)
	if err != nil {
		return fmt.Errorf("failed to confirm cancellation of order %s: %w", orderID, err)
	}
	if confirmed == nil || !isTerminalOrderStatus(confirmed.Status) || confirmed.Status == "filled" {
		return fmt.Errorf("cancellation of order %s is not terminal", orderID)
	}
	return nil
}

func (pm *PositionManager) cancelSiblingExitOrders(ctx context.Context, position *ManagedPosition, filledOrderID string) error {
	orderIDs := append([]string{position.StopLossOrderID, position.TakeProfitOrderID}, position.PartialExitOrders...)
	for _, orderID := range orderIDs {
		if orderID == "" || orderID == filledOrderID {
			continue
		}
		order, err := pm.tradingService.GetOrder(ctx, orderID)
		if err != nil {
			return fmt.Errorf("failed to inspect sibling order %s: %w", orderID, err)
		}
		if order != nil && order.Status == "filled" {
			continue
		}
		if err := pm.cancelAndConfirm(ctx, orderID); err != nil {
			return err
		}
	}
	return nil
}

func (pm *PositionManager) hasExposure(ctx context.Context, position *ManagedPosition) (bool, error) {
	positions, err := pm.tradingService.GetPositions(ctx)
	if err != nil {
		return true, err
	}
	for _, brokerPosition := range positions {
		if brokerPosition != nil && brokerPosition.Symbol == position.Symbol && math.Abs(brokerPosition.Qty) > 1e-9 {
			return true, nil
		}
	}
	return false, nil
}

func (pm *PositionManager) finishFilledExit(ctx context.Context, position *ManagedPosition, filledOrderID, terminalStatus string) error {
	if err := pm.cancelSiblingExitOrders(ctx, position, filledOrderID); err != nil {
		position.Status = "CLOSING"
		return err
	}
	exposed, err := pm.hasExposure(ctx, position)
	if err != nil {
		position.Status = "CLOSING"
		return err
	}
	if exposed || position.RemainingQty > 1e-9 {
		position.Status = "PARTIAL"
		return fmt.Errorf("broker still reports exposure after %s", filledOrderID)
	}
	position.Status = terminalStatus
	now := time.Now()
	position.ClosedAt = &now
	return nil
}

func (pm *PositionManager) reconcileClosing(ctx context.Context, position *ManagedPosition) error {
	if position.CloseOrderID == "" && position.CloseOrderClientOrderID != "" {
		order, err := pm.tradingService.GetOrderByClientOrderID(ctx, position.CloseOrderClientOrderID)
		if err != nil {
			return fmt.Errorf("close order lookup is ambiguous: %w", err)
		}
		if order == nil {
			return fmt.Errorf("close order lookup returned no result")
		}
		if isTerminalOrderStatus(order.Status) && order.Status != "filled" {
			// A confirmed terminal rejection/cancellation is safe to retry with a new
			// client id. Unknown outcomes never take this path.
			position.CloseOrderClientOrderID = ""
		} else {
			position.CloseOrderID = order.ID
		}
	}
	if position.CloseOrderID == "" {
		for _, orderID := range []string{position.EntryOrderID, position.StopLossOrderID, position.TakeProfitOrderID} {
			if err := pm.cancelAndConfirm(ctx, orderID); err != nil {
				return err
			}
		}
		for _, orderID := range position.PartialExitOrders {
			if err := pm.cancelAndConfirm(ctx, orderID); err != nil {
				return err
			}
		}
		clientOrderID, err := newClientOrderID()
		if err != nil {
			return fmt.Errorf("failed to generate close order id: %w", err)
		}
		position.CloseOrderClientOrderID = clientOrderID
		order := &interfaces.Order{
			ClientOrderID: clientOrderID,
			Symbol:        position.Symbol,
			Qty:           position.RemainingQty,
			Side:          map[string]string{"buy": "sell", "sell": "buy"}[position.Side],
			Type:          "market",
			TimeInForce:   "day",
			Status:        "pending",
			SubmittedAt:   time.Now(),
		}
		if err := pm.storageService.SaveOrder(order); err != nil {
			return fmt.Errorf("failed to persist close order intent: %w", err)
		}
		result, err := pm.tradingService.PlaceOrder(ctx, order)
		if err != nil {
			order.Status = "submit_failed"
			_ = pm.storageService.SaveOrder(order)
			_ = pm.savePositionToDB(position)
			return fmt.Errorf("close order outcome is unknown: %w", err)
		}
		order.ID = result.OrderID
		order.Status = result.Status
		position.CloseOrderID = result.OrderID
		if err := pm.storageService.SaveOrder(order); err != nil {
			return err
		}
	}

	order, err := pm.tradingService.GetOrder(ctx, position.CloseOrderID)
	if err != nil {
		return fmt.Errorf("failed to reconcile close order: %w", err)
	}
	if order == nil {
		return fmt.Errorf("broker returned no close order")
	}
	if order.Status == "filled" {
		position.RemainingQty = math.Max(0, position.RemainingQty-order.FilledQty)
		if order.FilledQty == 0 {
			position.RemainingQty = 0
		}
		return pm.finishFilledExit(ctx, position, position.CloseOrderID, "CLOSED")
	}
	if isTerminalOrderStatus(order.Status) {
		position.CloseOrderID = ""
		position.Status = "ACTIVE"
		if position.RemainingQty < position.Quantity {
			position.Status = "PARTIAL"
		}
		return fmt.Errorf("close order ended with status %s; retry is required", order.Status)
	}
	return nil
}

// manageRiskOrders checks and updates risk management orders
func (pm *PositionManager) manageRiskOrders(ctx context.Context, position *ManagedPosition) {
	if position.Status == "CLOSING" {
		if position.CloseOrderID == "" && position.CloseOrderClientOrderID == "" && position.RemainingQty <= 1e-9 {
			exitOrderIDs := []string{position.StopLossOrderID, position.TakeProfitOrderID}
			exitOrderIDs = append(exitOrderIDs, position.PartialExitOrders...)
			for _, orderID := range exitOrderIDs {
				if orderID == "" {
					continue
				}
				order, err := pm.tradingService.GetOrder(ctx, orderID)
				if err == nil && order != nil && order.Status == "filled" {
					terminalStatus := "CLOSED"
					if orderID == position.StopLossOrderID {
						terminalStatus = "STOPPED_OUT"
					}
					if err := pm.finishFilledExit(ctx, position, orderID, terminalStatus); err != nil {
						pm.logger.WithError(err).WithField("position_id", position.ID).Warn("Filled risk order still requires reconciliation")
					}
					_ = pm.savePositionToDB(position)
					return
				}
			}
		}
		if err := pm.reconcileClosing(ctx, position); err != nil {
			pm.logger.WithError(err).WithField("position_id", position.ID).Warn("Position close still requires reconciliation")
		}
		_ = pm.savePositionToDB(position)
		return
	}
	if position.ProcessedExitFills == nil {
		position.ProcessedExitFills = make(map[string]float64)
	}
	filledOrderID := ""
	filledTerminalStatus := "CLOSED"
	changed := false
	orderIDs := []string{position.StopLossOrderID, position.TakeProfitOrderID}
	orderIDs = append(orderIDs, position.PartialExitOrders...)
	for _, orderID := range orderIDs {
		if orderID == "" {
			continue
		}
		order, err := pm.tradingService.GetOrder(ctx, orderID)
		if err != nil || order == nil || (order.Status != "filled" && order.Status != "partially_filled") {
			continue
		}
		filledQty := math.Max(0, order.FilledQty)
		if order.Status == "filled" && filledQty == 0 {
			filledQty = order.Qty
		}
		delta := math.Max(0, filledQty-position.ProcessedExitFills[orderID])
		if delta == 0 {
			if order.Status == "filled" && position.RemainingQty <= 1e-9 && filledOrderID == "" {
				filledOrderID = orderID
				if orderID == position.StopLossOrderID {
					filledTerminalStatus = "STOPPED_OUT"
				}
			}
			continue
		}
		position.ProcessedExitFills[orderID] = filledQty
		position.RemainingQty = math.Max(0, position.RemainingQty-delta)
		changed = true
		if order.Status == "filled" && position.RemainingQty <= 1e-9 {
			filledOrderID = orderID
			if orderID == position.StopLossOrderID {
				filledTerminalStatus = "STOPPED_OUT"
			}
		}
	}
	if position.RemainingQty <= 1e-9 && filledOrderID != "" {
		position.Status = "CLOSING"
		if err := pm.finishFilledExit(ctx, position, filledOrderID, filledTerminalStatus); err != nil {
			pm.logger.WithError(err).WithField("position_id", position.ID).Warn("Filled risk order requires reconciliation")
		}
	} else if changed {
		position.Status = "PARTIAL"
		if err := pm.resizeProtection(ctx, position); err != nil {
			pm.logger.WithError(err).WithField("position_id", position.ID).Warn("Failed to resize protection after partial fill")
		}
	}
	if changed || position.Status == "PARTIAL" {
		_ = pm.savePositionToDB(position)
	}
}

func (pm *PositionManager) resizeProtection(ctx context.Context, position *ManagedPosition) error {
	if position.RemainingQty <= 1e-9 {
		return nil
	}
	if position.StopLossOrderID != "" {
		order, err := pm.tradingService.GetOrder(ctx, position.StopLossOrderID)
		if err != nil || order == nil {
			return fmt.Errorf("failed to inspect stop loss order for resize")
		}
		if order.Status != "filled" && math.Abs(order.Qty-position.RemainingQty) > 1e-9 {
			if err := pm.cancelAndConfirm(ctx, position.StopLossOrderID); err != nil {
				return err
			}
			position.StopLossOrderID = ""
		}
	}
	if position.TakeProfitOrderID != "" {
		order, err := pm.tradingService.GetOrder(ctx, position.TakeProfitOrderID)
		if err != nil || order == nil {
			return fmt.Errorf("failed to inspect take profit order for resize")
		}
		if order.Status != "filled" && math.Abs(order.Qty-position.RemainingQty) > 1e-9 {
			if err := pm.cancelAndConfirm(ctx, position.TakeProfitOrderID); err != nil {
				return err
			}
			position.TakeProfitOrderID = ""
		}
	}
	if position.StopLossOrderID == "" {
		if err := pm.placeStopLossOrder(ctx, position); err != nil {
			return err
		}
	}
	if position.TakeProfitOrderID == "" {
		if err := pm.placeTakeProfitOrder(ctx, position); err != nil {
			return err
		}
	}
	return nil
}

func (pm *PositionManager) replaceTrailingStop(ctx context.Context, position *ManagedPosition, newStopPrice float64) error {
	oldOrderID, oldStopPrice := position.StopLossOrderID, position.StopLossPrice
	// Submit the replacement first. The old protection remains executable until the
	// replacement is accepted, so a failed replacement cannot leave naked exposure.
	replacement := *position
	replacement.StopLossOrderID = ""
	replacement.StopLossPrice = newStopPrice
	if err := pm.placeStopLossOrder(ctx, &replacement); err != nil {
		return err
	}
	newOrderID := replacement.StopLossOrderID
	if oldOrderID != "" {
		if err := pm.cancelAndConfirm(ctx, oldOrderID); err != nil {
			if cancelErr := pm.cancelAndConfirm(ctx, newOrderID); cancelErr != nil {
				return fmt.Errorf("trailing replacement and rollback are ambiguous: %w; rollback: %v", err, cancelErr)
			}
			return err
		}
	}
	position.StopLossOrderID = newOrderID
	position.StopLossPrice = newStopPrice
	position.UpdatedAt = time.Now()
	_ = oldStopPrice // retained above to make the rollback state explicit in logs/debuggers
	return pm.savePositionToDB(position)
}

// updateTrailingStop updates trailing stop loss based on current price
func (pm *PositionManager) updateTrailingStop(ctx context.Context, position *ManagedPosition) {
	if position.Status != "ACTIVE" && position.Status != "PARTIAL" {
		return
	}
	newStopPrice := position.CurrentPrice * (1 - position.TrailingPercent/100.0)
	if position.Side == "sell" {
		newStopPrice = position.CurrentPrice * (1 + position.TrailingPercent/100.0)
	}
	shouldMove := position.Side == "buy" && newStopPrice > position.StopLossPrice
	if position.Side == "sell" {
		shouldMove = newStopPrice < position.StopLossPrice
	}
	if !shouldMove {
		return
	}
	if err := pm.replaceTrailingStop(ctx, position, newStopPrice); err != nil {
		pm.logger.WithError(err).WithField("position_id", position.ID).Warn("Trailing stop replacement requires reconciliation")
		position.Status = "CLOSING"
		_ = pm.savePositionToDB(position)
		return
	}
	pm.logger.WithFields(logrus.Fields{
		"position_id":    position.ID,
		"new_stop_price": newStopPrice,
	}).Info("Trailing stop updated")
}

// updatePositionPrice updates current price and unrealized P&L
func (pm *PositionManager) updatePositionPrice(ctx context.Context, position *ManagedPosition) error {
	currentPrice, err := pm.getCurrentPrice(ctx, position.Symbol)
	if err != nil {
		return err
	}

	position.CurrentPrice = currentPrice

	if position.Side == "buy" {
		position.UnrealizedPL = (currentPrice - position.EntryPrice) * position.RemainingQty
		position.UnrealizedPLPC = ((currentPrice - position.EntryPrice) / position.EntryPrice) * 100
	} else {
		position.UnrealizedPL = (position.EntryPrice - currentPrice) * position.RemainingQty
		position.UnrealizedPLPC = ((position.EntryPrice - currentPrice) / position.EntryPrice) * 100
	}

	position.UpdatedAt = time.Now()

	return nil
}

// GetManagedPosition retrieves a managed position by ID
func (pm *PositionManager) GetManagedPosition(positionID string) (*ManagedPosition, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	position, exists := pm.positions[positionID]
	if !exists {
		return nil, fmt.Errorf("position not found: %s", positionID)
	}

	return position, nil
}

// ListManagedPositions returns all managed positions
// Filters out PENDING positions older than 24 hours (stale orders)
func (pm *PositionManager) ListManagedPositions(status string) []*ManagedPosition {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	positions := make([]*ManagedPosition, 0)
	now := time.Now()

	for _, pos := range pm.positions {
		// Filter out stale PENDING orders (>24 hours old)
		if pos.Status == "PENDING" {
			age := now.Sub(pos.CreatedAt)
			if age > 24*time.Hour {
				pm.logger.WithFields(logrus.Fields{
					"position_id": pos.ID,
					"symbol":      pos.Symbol,
					"age_hours":   age.Hours(),
				}).Debug("Skipping stale PENDING position")
				continue
			}
		}

		if status == "" || pos.Status == status {
			positions = append(positions, pos)
		}
	}

	return positions
}

// CloseManagedPosition manually closes a managed position
func (pm *PositionManager) CloseManagedPosition(ctx context.Context, positionID string) error {
	pm.closeMu.Lock()
	defer pm.closeMu.Unlock()
	pm.mu.Lock()
	position, exists := pm.positions[positionID]
	if exists && position.Status != "CLOSED" && position.Status != "STOPPED_OUT" && position.Status != "CLOSING" {
		position.Status = "CLOSING"
		position.UpdatedAt = time.Now()
	}
	pm.mu.Unlock()

	if !exists {
		return fmt.Errorf("position not found: %s", positionID)
	}
	if position.Status == "CLOSED" || position.Status == "STOPPED_OUT" {
		return nil
	}
	if err := pm.savePositionToDB(position); err != nil {
		return fmt.Errorf("failed to persist closing state: %w", err)
	}
	if position.EntryOrderID != "" {
		entry, err := pm.tradingService.GetOrder(ctx, position.EntryOrderID)
		if err != nil || entry == nil {
			return fmt.Errorf("failed to reconcile entry order before closing")
		}
		if entry.Status == "filled" {
			position.Status = "CLOSING"
		} else if entry.Status == "partially_filled" && entry.FilledQty > 0 {
			position.RemainingQty = math.Min(position.RemainingQty, entry.FilledQty)
			if err := pm.cancelAndConfirm(ctx, position.EntryOrderID); err != nil {
				_ = pm.savePositionToDB(position)
				return err
			}
			position.Status = "CLOSING"
		} else if err := pm.cancelAndConfirm(ctx, position.EntryOrderID); err != nil {
			_ = pm.savePositionToDB(position)
			return err
		} else {
			position.Status = "CLOSED"
			now := time.Now()
			position.ClosedAt = &now
			_ = pm.savePositionToDB(position)
			return nil
		}
	}
	if err := pm.reconcileClosing(ctx, position); err != nil {
		_ = pm.savePositionToDB(position)
		return err
	}
	if err := pm.savePositionToDB(position); err != nil {
		return err
	}
	return nil
}

// Helper functions

func (pm *PositionManager) validateRequest(req *PlaceManagedPositionRequest) error {
	if req.Side != "buy" && req.Side != "sell" {
		return fmt.Errorf("side must be 'buy' or 'sell'")
	}

	if req.EntryStrategy == "limit" && req.EntryPrice == nil {
		return fmt.Errorf("entry_price required for limit orders")
	}

	if req.StopLossPrice == nil && req.StopLossPercent == nil {
		return fmt.Errorf("either stop_loss_price or stop_loss_percent required")
	}

	if req.TakeProfitPrice == nil && req.TakeProfitPercent == nil {
		return fmt.Errorf("either take_profit_price or take_profit_percent required")
	}

	return nil
}

func (pm *PositionManager) getCurrentPrice(ctx context.Context, symbol string) (float64, error) {
	quote, err := pm.dataService.GetLatestQuote(ctx, symbol)
	if err != nil {
		return 0, err
	}

	if quote.AskPrice > 0 {
		return quote.AskPrice, nil
	}

	return quote.BidPrice, nil
}

func (pm *PositionManager) calculateQuantity(allocation, price float64) float64 {
	return math.Floor(allocation / price)
}

func (pm *PositionManager) calculateStopLoss(entryPrice float64, stopPrice *float64, stopPercent *float64, side string) float64 {
	if stopPrice != nil {
		return *stopPrice
	}

	if side == "buy" {
		return entryPrice * (1 - *stopPercent/100.0)
	}

	return entryPrice * (1 + *stopPercent/100.0)
}

func (pm *PositionManager) calculateTakeProfit(entryPrice float64, profitPrice *float64, profitPercent *float64, side string) float64 {
	if profitPrice != nil {
		return *profitPrice
	}

	if side == "buy" {
		return entryPrice * (1 + *profitPercent/100.0)
	}

	return entryPrice * (1 - *profitPercent/100.0)
}

func (pm *PositionManager) calculatePartialExitPrice(entryPrice, targetPercent float64, side string) float64 {
	if side == "buy" {
		return entryPrice * (1 + targetPercent/100.0)
	}

	return entryPrice * (1 - targetPercent/100.0)
}

func (pm *PositionManager) generatePositionID() string {
	return fmt.Sprintf("pos_%d", time.Now().UnixNano())
}

// Stop stops the position manager
func (pm *PositionManager) Stop() {
	pm.cancel()
}

// loadPositionsFromDB loads all active positions from database on startup
func (pm *PositionManager) loadPositionsFromDB() error {
	// Load all non-closed positions
	dbPositions, err := pm.storageService.GetAllManagedPositions("")
	if err != nil {
		return err
	}

	loaded := 0
	for _, dbPos := range dbPositions {
		// Skip closed positions
		if dbPos.Status == "CLOSED" || dbPos.Status == "STOPPED_OUT" {
			continue
		}

		// Convert DB position to managed position
		position := pm.dbToManagedPosition(dbPos)

		// Store in memory
		pm.positions[position.ID] = position
		loaded++
	}

	pm.logger.WithField("count", loaded).Info("Loaded managed positions from database")
	return nil
}

// savePositionToDB saves a managed position to database
func (pm *PositionManager) savePositionToDB(position *ManagedPosition) error {
	dbPosition := pm.managedPositionToDB(position)
	return pm.storageService.SaveManagedPosition(dbPosition)
}

// managedPositionToDB converts ManagedPosition to DBManagedPosition
func (pm *PositionManager) managedPositionToDB(pos *ManagedPosition) *models.DBManagedPosition {
	// Convert partial exit orders to JSON
	partialExitOrdersJSON, _ := json.Marshal(pos.PartialExitOrders)

	// Convert tags to JSON
	tagsJSON, _ := json.Marshal(pos.Tags)
	processedExitFillsJSON, _ := json.Marshal(pos.ProcessedExitFills)

	dbPos := &models.DBManagedPosition{
		PositionID:              pos.ID,
		Symbol:                  pos.Symbol,
		Side:                    pos.Side,
		Strategy:                pos.Strategy,
		Quantity:                pos.Quantity,
		EntryPrice:              pos.EntryPrice,
		EntryOrderID:            pos.EntryOrderID,
		EntryOrderType:          pos.EntryOrderType,
		AllocationDollars:       pos.AllocationDollars,
		StopLossPrice:           pos.StopLossPrice,
		StopLossPercent:         pos.StopLossPercent,
		StopLossOrderID:         pos.StopLossOrderID,
		TrailingStop:            pos.TrailingStop,
		TrailingPercent:         pos.TrailingPercent,
		TakeProfitPrice:         pos.TakeProfitPrice,
		TakeProfitPercent:       pos.TakeProfitPercent,
		TakeProfitOrderID:       pos.TakeProfitOrderID,
		CloseOrderID:            pos.CloseOrderID,
		CloseOrderClientOrderID: pos.CloseOrderClientOrderID,
		Status:                  pos.Status,
		CurrentPrice:            pos.CurrentPrice,
		UnrealizedPL:            pos.UnrealizedPL,
		UnrealizedPLPC:          pos.UnrealizedPLPC,
		RemainingQty:            pos.RemainingQty,
		Notes:                   pos.Notes,
		Tags:                    string(tagsJSON),
		PartialExitOrders:       string(partialExitOrdersJSON),
		ProcessedExitFills:      string(processedExitFillsJSON),
		ClosedAt:                pos.ClosedAt,
	}

	if pos.PartialExit != nil {
		dbPos.PartialExitEnabled = pos.PartialExit.Enabled
		dbPos.PartialExitPercent = pos.PartialExit.Percent
		dbPos.PartialExitTargetPercent = pos.PartialExit.TargetPercent
		dbPos.PartialExitTargetPrice = pos.PartialExit.TargetPrice
	}

	return dbPos
}

// dbToManagedPosition converts DBManagedPosition to ManagedPosition
func (pm *PositionManager) dbToManagedPosition(dbPos *models.DBManagedPosition) *ManagedPosition {
	// Parse partial exit orders from JSON
	var partialExitOrders []string
	if dbPos.PartialExitOrders != "" {
		json.Unmarshal([]byte(dbPos.PartialExitOrders), &partialExitOrders)
	}

	// Parse tags from JSON
	var tags []string
	if dbPos.Tags != "" {
		json.Unmarshal([]byte(dbPos.Tags), &tags)
	}
	var processedExitFills map[string]float64
	if dbPos.ProcessedExitFills != "" {
		json.Unmarshal([]byte(dbPos.ProcessedExitFills), &processedExitFills)
	}
	if processedExitFills == nil {
		processedExitFills = make(map[string]float64)
	}

	pos := &ManagedPosition{
		ID:                      dbPos.PositionID,
		Symbol:                  dbPos.Symbol,
		Side:                    dbPos.Side,
		Strategy:                dbPos.Strategy,
		Quantity:                dbPos.Quantity,
		EntryPrice:              dbPos.EntryPrice,
		EntryOrderID:            dbPos.EntryOrderID,
		EntryOrderType:          dbPos.EntryOrderType,
		AllocationDollars:       dbPos.AllocationDollars,
		StopLossPrice:           dbPos.StopLossPrice,
		StopLossPercent:         dbPos.StopLossPercent,
		StopLossOrderID:         dbPos.StopLossOrderID,
		TrailingStop:            dbPos.TrailingStop,
		TrailingPercent:         dbPos.TrailingPercent,
		TakeProfitPrice:         dbPos.TakeProfitPrice,
		TakeProfitPercent:       dbPos.TakeProfitPercent,
		TakeProfitOrderID:       dbPos.TakeProfitOrderID,
		CloseOrderID:            dbPos.CloseOrderID,
		CloseOrderClientOrderID: dbPos.CloseOrderClientOrderID,
		Status:                  dbPos.Status,
		CurrentPrice:            dbPos.CurrentPrice,
		UnrealizedPL:            dbPos.UnrealizedPL,
		UnrealizedPLPC:          dbPos.UnrealizedPLPC,
		RemainingQty:            dbPos.RemainingQty,
		ProcessedExitFills:      processedExitFills,
		Notes:                   dbPos.Notes,
		Tags:                    tags,
		PartialExitOrders:       partialExitOrders,
		CreatedAt:               dbPos.CreatedAt,
		UpdatedAt:               dbPos.UpdatedAt,
		ClosedAt:                dbPos.ClosedAt,
	}

	if dbPos.PartialExitEnabled {
		pos.PartialExit = &PartialExitConfig{
			Enabled:       dbPos.PartialExitEnabled,
			Percent:       dbPos.PartialExitPercent,
			TargetPercent: dbPos.PartialExitTargetPercent,
			TargetPrice:   dbPos.PartialExitTargetPrice,
		}
	}

	return pos
}
