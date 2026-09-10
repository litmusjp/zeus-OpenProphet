package services

import (
	"context"
	"errors"
	"path/filepath"
	"prophet-trader/database"
	"prophet-trader/interfaces"
	"testing"
	"time"
)

type exitOrderRecorder struct {
	placed        *interfaces.Order
	placedOrders  []*interfaces.Order
	result        *interfaces.OrderResult
	results       []*interfaces.OrderResult
	placeErr      error
	orders        map[string]*interfaces.Order
	positions     []*interfaces.Position
	cancelCount   int
	cancelErr     error
	cancelPending bool
}

func (s *exitOrderRecorder) PlaceOrder(_ context.Context, o *interfaces.Order) (*interfaces.OrderResult, error) {
	s.placed = o
	s.placedOrders = append(s.placedOrders, o)
	if s.placeErr != nil {
		return nil, s.placeErr
	}
	result := s.result
	if len(s.results) > 0 {
		result = s.results[0]
		s.results = s.results[1:]
	}
	if result != nil {
		if s.orders == nil {
			s.orders = make(map[string]*interfaces.Order)
		}
		stored := *o
		stored.ID = result.OrderID
		stored.Status = result.Status
		s.orders[result.OrderID] = &stored
	}
	return result, nil
}
func (s *exitOrderRecorder) CancelOrder(_ context.Context, orderID string) error {
	s.cancelCount++
	if s.cancelErr != nil {
		return s.cancelErr
	}
	if order, ok := s.orders[orderID]; ok {
		if !s.cancelPending {
			order.Status = "canceled"
		}
	}
	return nil
}
func (s *exitOrderRecorder) GetOrder(_ context.Context, orderID string) (*interfaces.Order, error) {
	if order, ok := s.orders[orderID]; ok {
		copy := *order
		return &copy, nil
	}
	return nil, errors.New("order not found")
}
func (s *exitOrderRecorder) GetOrderByClientOrderID(_ context.Context, clientOrderID string) (*interfaces.Order, error) {
	for _, order := range s.orders {
		if order.ClientOrderID == clientOrderID {
			copy := *order
			return &copy, nil
		}
	}
	return nil, errors.New("order not found")
}
func (s *exitOrderRecorder) ListOrders(context.Context, string) ([]*interfaces.Order, error) {
	return nil, nil
}
func (s *exitOrderRecorder) GetPositions(context.Context) ([]*interfaces.Position, error) {
	return s.positions, nil
}
func (s *exitOrderRecorder) GetAccount(context.Context) (*interfaces.Account, error) {
	return nil, nil
}
func (s *exitOrderRecorder) PlaceOptionsOrder(context.Context, *interfaces.OptionsOrder) (*interfaces.OrderResult, error) {
	return nil, nil
}
func (s *exitOrderRecorder) GetOptionsChain(context.Context, string, time.Time) ([]*interfaces.OptionContract, error) {
	return nil, nil
}
func (s *exitOrderRecorder) GetOptionsQuote(context.Context, string) (*interfaces.OptionsQuote, error) {
	return nil, nil
}
func (s *exitOrderRecorder) GetOptionsPosition(context.Context, string) (*interfaces.OptionsPosition, error) {
	return nil, nil
}
func (s *exitOrderRecorder) ListOptionsPositions(context.Context) ([]*interfaces.OptionsPosition, error) {
	return nil, nil
}

func newTestPositionManager(t *testing.T, rec *exitOrderRecorder) (*PositionManager, *database.LocalStorage) {
	t.Helper()
	storage, err := database.NewLocalStorage(filepath.Join(t.TempDir(), "pm.db"))
	if err != nil {
		t.Fatalf("NewLocalStorage() error = %v", err)
	}
	return NewPositionManager(rec, nil, storage), storage
}

func TestManagedProtectiveLegsPersistIntentBeforeSubmit(t *testing.T) {
	cases := []struct {
		name     string
		position func() *ManagedPosition
		place    func(*PositionManager, *ManagedPosition) error
	}{
		{
			name: "stop_loss",
			position: func() *ManagedPosition {
				return &ManagedPosition{ID: "p1", Symbol: "AAPL", Side: "buy", RemainingQty: 10, StopLossPrice: 90}
			},
			place: func(pm *PositionManager, pos *ManagedPosition) error {
				return pm.placeStopLossOrder(context.Background(), pos)
			},
		},
		{
			name: "take_profit",
			position: func() *ManagedPosition {
				return &ManagedPosition{ID: "p2", Symbol: "AAPL", Side: "buy", RemainingQty: 10, TakeProfitPrice: 110}
			},
			place: func(pm *PositionManager, pos *ManagedPosition) error {
				return pm.placeTakeProfitOrder(context.Background(), pos)
			},
		},
		{
			name: "partial_exit",
			position: func() *ManagedPosition {
				return &ManagedPosition{ID: "p3", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 10, PartialExit: &PartialExitConfig{Enabled: true, Percent: 50, TargetPrice: 105}}
			},
			place: func(pm *PositionManager, pos *ManagedPosition) error {
				return pm.placePartialExitOrder(context.Background(), pos)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/submit_fails", func(t *testing.T) {
			rec := &exitOrderRecorder{placeErr: errors.New("broker timeout")}
			pm, storage := newTestPositionManager(t, rec)
			defer storage.Close()

			if err := tc.place(pm, tc.position()); err == nil {
				t.Fatal("expected an error when the broker submit fails")
			}
			if rec.placed == nil || rec.placed.ClientOrderID == "" {
				t.Fatalf("PlaceOrder should receive an order carrying a ClientOrderID, got %#v", rec.placed)
			}
			failed, err := storage.GetOrders("submit_failed")
			if err != nil {
				t.Fatalf("GetOrders(submit_failed) error = %v", err)
			}
			if len(failed) != 1 || failed[0].ClientOrderID != rec.placed.ClientOrderID {
				t.Fatalf("expected 1 submit_failed intent persisted with the broker client id, got %#v", failed)
			}
		})

		t.Run(tc.name+"/submit_ok", func(t *testing.T) {
			rec := &exitOrderRecorder{result: &interfaces.OrderResult{OrderID: "broker-" + tc.name, Status: "accepted"}}
			pm, storage := newTestPositionManager(t, rec)
			defer storage.Close()

			if err := tc.place(pm, tc.position()); err != nil {
				t.Fatalf("place() unexpected error = %v", err)
			}
			saved, err := storage.GetOrder("broker-" + tc.name)
			if err != nil {
				t.Fatalf("GetOrder() error = %v", err)
			}
			if saved.Status != "accepted" || saved.ClientOrderID == "" {
				t.Fatalf("saved order = %#v, want accepted status + a ClientOrderID", saved)
			}
			all, err := storage.GetOrders("")
			if err != nil {
				t.Fatalf("GetOrders() error = %v", err)
			}
			if len(all) != 1 {
				t.Fatalf("expected exactly 1 row after pre-submit + post-submit upsert, got %d", len(all))
			}
		})
	}
}

func TestCloseManagedPositionPersistsExitIntentOnAmbiguousSubmit(t *testing.T) {
	rec := &exitOrderRecorder{placeErr: errors.New("market closed")}
	pm, storage := newTestPositionManager(t, rec)
	defer storage.Close()

	pos := &ManagedPosition{ID: "c1", Symbol: "AAPL", Side: "buy", Status: "ACTIVE", RemainingQty: 10}
	pm.positions[pos.ID] = pos

	if err := pm.CloseManagedPosition(context.Background(), pos.ID); err == nil {
		t.Fatal("expected the ambiguous exit submission to be reported to the caller")
	}
	if pos.Status != "CLOSING" {
		t.Fatalf("position status = %q, want CLOSING until broker outcome is reconciled", pos.Status)
	}
	if rec.placed == nil || rec.placed.ClientOrderID == "" {
		t.Fatalf("exit PlaceOrder should carry a ClientOrderID, got %#v", rec.placed)
	}
	failed, err := storage.GetOrders("submit_failed")
	if err != nil {
		t.Fatalf("GetOrders(submit_failed) error = %v", err)
	}
	if len(failed) != 1 || failed[0].ClientOrderID != rec.placed.ClientOrderID {
		t.Fatalf("expected the ambiguous exit persisted as submit_failed for reconciliation, got %#v", failed)
	}
}

func TestCloseManagedPositionKeepsAcceptedExitClosingAndIsIdempotent(t *testing.T) {
	rec := &exitOrderRecorder{result: &interfaces.OrderResult{OrderID: "close-1", Status: "accepted"}, orders: make(map[string]*interfaces.Order)}
	pm, storage := newTestPositionManager(t, rec)
	defer storage.Close()
	pos := &ManagedPosition{ID: "c2", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 10, Status: "ACTIVE"}
	pm.positions[pos.ID] = pos

	if err := pm.CloseManagedPosition(context.Background(), pos.ID); err != nil {
		t.Fatalf("accepted close should be recorded without claiming it is filled: %v", err)
	}
	if pos.Status != "CLOSING" || len(rec.placedOrders) != 1 {
		t.Fatalf("position=%q placed=%d, want CLOSING and one exit", pos.Status, len(rec.placedOrders))
	}
	if err := pm.CloseManagedPosition(context.Background(), pos.ID); err != nil {
		t.Fatalf("retry while accepted should reconcile, not submit another exit: %v", err)
	}
	if len(rec.placedOrders) != 1 {
		t.Fatalf("retry created a duplicate exit: %d placements", len(rec.placedOrders))
	}

	rec.orders["close-1"].Status = "filled"
	rec.orders["close-1"].FilledQty = 10
	pm.manageRiskOrders(context.Background(), pos)
	if pos.Status != "CLOSED" {
		t.Fatalf("position status = %q, want CLOSED after confirmed fill and flat broker exposure", pos.Status)
	}
}

func TestPartialExitFillsAreCumulativeAndResizeProtection(t *testing.T) {
	rec := &exitOrderRecorder{
		results: []*interfaces.OrderResult{{OrderID: "new-stop", Status: "accepted"}, {OrderID: "new-target", Status: "accepted"}},
		orders: map[string]*interfaces.Order{
			"stop":    {ID: "stop", Symbol: "AAPL", Qty: 10, Status: "accepted"},
			"target":  {ID: "target", Symbol: "AAPL", Qty: 10, Status: "accepted"},
			"partial": {ID: "partial", Symbol: "AAPL", Qty: 5, Status: "filled", FilledQty: 3},
		},
	}
	pm, storage := newTestPositionManager(t, rec)
	defer storage.Close()
	pos := &ManagedPosition{ID: "p4", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 10, Status: "ACTIVE", StopLossOrderID: "stop", TakeProfitOrderID: "target", PartialExitOrders: []string{"partial"}}
	pm.manageRiskOrders(context.Background(), pos)
	if pos.Status != "PARTIAL" || pos.RemainingQty != 7 {
		t.Fatalf("after first partial fill position=%q remaining=%v, want PARTIAL/7", pos.Status, pos.RemainingQty)
	}
	pm.manageRiskOrders(context.Background(), pos)
	if pos.RemainingQty != 7 {
		t.Fatalf("repeated poll double-counted cumulative fill: remaining=%v", pos.RemainingQty)
	}
	if pos.StopLossOrderID != "new-stop" || pos.TakeProfitOrderID != "new-target" {
		t.Fatalf("protection IDs = %q/%q, want resized replacement orders", pos.StopLossOrderID, pos.TakeProfitOrderID)
	}
}

func TestManagedPositionCloseStateAndFillLedgerSurviveRestart(t *testing.T) {
	rec := &exitOrderRecorder{}
	pm, storage := newTestPositionManager(t, rec)
	pos := &ManagedPosition{ID: "restart-1", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 7, Status: "CLOSING", CloseOrderID: "close-7", CloseOrderClientOrderID: "client-7", ProcessedExitFills: map[string]float64{"partial-1": 3}}
	if err := pm.savePositionToDB(pos); err != nil {
		t.Fatalf("savePositionToDB() error = %v", err)
	}
	pm2 := NewPositionManager(rec, nil, storage)
	loaded := pm2.positions[pos.ID]
	if loaded == nil || loaded.Status != "CLOSING" || loaded.CloseOrderID != "close-7" || loaded.ProcessedExitFills["partial-1"] != 3 {
		t.Fatalf("restarted position = %#v, expected close state and fill ledger", loaded)
	}
	storage.Close()
}

func TestFilledExitWaitsForSiblingCancellationToBecomeTerminal(t *testing.T) {
	rec := &exitOrderRecorder{
		cancelPending: true,
		orders: map[string]*interfaces.Order{
			"stop":   {ID: "stop", Symbol: "AAPL", Qty: 10, Status: "accepted"},
			"target": {ID: "target", Symbol: "AAPL", Qty: 10, Status: "filled", FilledQty: 10},
		},
	}
	pm, storage := newTestPositionManager(t, rec)
	defer storage.Close()
	pos := &ManagedPosition{ID: "sibling-1", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 10, Status: "ACTIVE", StopLossOrderID: "stop", TakeProfitOrderID: "target"}

	pm.manageRiskOrders(context.Background(), pos)
	if pos.Status == "CLOSED" || pos.Status == "STOPPED_OUT" {
		t.Fatalf("position became terminal before sibling cancellation settled: %q", pos.Status)
	}
	rec.cancelPending = false
	pm.manageRiskOrders(context.Background(), pos)
	if pos.Status != "CLOSED" {
		t.Fatalf("position status = %q, want CLOSED after sibling cancellation confirmation", pos.Status)
	}
}

func TestTrailingReplacementFailureRetainsExistingProtection(t *testing.T) {
	rec := &exitOrderRecorder{
		placeErr: errors.New("replacement rejected"),
		orders: map[string]*interfaces.Order{
			"old-stop": {ID: "old-stop", Symbol: "AAPL", Qty: 10, Status: "accepted"},
		},
	}
	pm, storage := newTestPositionManager(t, rec)
	defer storage.Close()
	pos := &ManagedPosition{ID: "trail-1", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 10, Status: "ACTIVE", StopLossOrderID: "old-stop", StopLossPrice: 90, CurrentPrice: 100, TrailingStop: true, TrailingPercent: 5}

	pm.updateTrailingStop(context.Background(), pos)
	if pos.StopLossOrderID != "old-stop" {
		t.Fatalf("old protection ID changed after failed replacement: %q", pos.StopLossOrderID)
	}
	if rec.cancelCount != 0 {
		t.Fatalf("old protection was canceled before replacement succeeded")
	}
	if pos.Status != "CLOSING" {
		t.Fatalf("status = %q, want CLOSING for replacement reconciliation", pos.Status)
	}
}

func TestLaterProtectiveFillClosesResidualAfterPartialExit(t *testing.T) {
	rec := &exitOrderRecorder{orders: map[string]*interfaces.Order{
		"stop":    {ID: "stop", Symbol: "AAPL", Qty: 7, Status: "accepted"},
		"target":  {ID: "target", Symbol: "AAPL", Qty: 7, Status: "accepted"},
		"partial": {ID: "partial", Symbol: "AAPL", Qty: 5, Status: "filled", FilledQty: 3},
	}}
	pm, storage := newTestPositionManager(t, rec)
	defer storage.Close()
	pos := &ManagedPosition{ID: "residual-1", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 7, Status: "PARTIAL", StopLossOrderID: "stop", TakeProfitOrderID: "target", PartialExitOrders: []string{"partial"}, ProcessedExitFills: map[string]float64{"partial": 3}}
	rec.orders["target"].Status = "filled"
	rec.orders["target"].FilledQty = 7

	pm.manageRiskOrders(context.Background(), pos)
	if pos.Status != "CLOSED" {
		t.Fatalf("residual target fill left position in %q, want CLOSED", pos.Status)
	}
}

func TestPartiallyFilledEntryRemainsTrackedUntilComplete(t *testing.T) {
	avg := 101.0
	rec := &exitOrderRecorder{
		results: []*interfaces.OrderResult{
			{OrderID: "stop-4", Status: "accepted"}, {OrderID: "target-4", Status: "accepted"},
			{OrderID: "stop-10", Status: "accepted"}, {OrderID: "target-10", Status: "accepted"},
		},
		orders: map[string]*interfaces.Order{
			"entry": {ID: "entry", Symbol: "AAPL", Qty: 10, Status: "partially_filled", FilledQty: 4, FilledAvgPrice: &avg},
		},
	}
	pm, storage := newTestPositionManager(t, rec)
	defer storage.Close()
	pos := &ManagedPosition{ID: "entry-1", Symbol: "AAPL", Side: "buy", Quantity: 10, RemainingQty: 10, Status: "PENDING", EntryOrderID: "entry", StopLossPrice: 90, TakeProfitPrice: 110}

	pm.checkEntryOrder(context.Background(), pos)
	if pos.Status != "PENDING" || pos.RemainingQty != 4 {
		t.Fatalf("partial entry state = %q/%v, want PENDING/4", pos.Status, pos.RemainingQty)
	}
	rec.orders["entry"].Status = "filled"
	rec.orders["entry"].FilledQty = 10
	pm.checkEntryOrder(context.Background(), pos)
	if pos.Status != "ACTIVE" || pos.RemainingQty != 10 {
		t.Fatalf("completed entry state = %q/%v, want ACTIVE/10", pos.Status, pos.RemainingQty)
	}
}
