package repository

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"yingce/backend/internal/model"
)

func TestAudioBillingAmountRoundsUp(t *testing.T) {
	amount, err := audioBillingAmount(100, 3, 10_000)
	if err != nil || amount != 300 {
		t.Fatalf("audioBillingAmount() = %d, %v; want 300", amount, err)
	}
}

func TestSettleAudioPerSecondOrderUsesQuotedAmount(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-settle?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.CreditAccount{}, &model.BillingOrder{}, &model.CreditLedgerEntry{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.CreditAccount{UserID: "user-audio", AvailableMicrocredits: 1_000, ReservedMicrocredits: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "audio-order", UserID: "user-audio", IdempotencyKey: "audio-task", Capability: "audio", BillingMode: "per_second",
		UnitPriceMicrocredits: 100, MultiplierBasisPoints: 10_000, Quantity: 1, AmountMicrocredits: 100,
		BillingCostSnapshot:        model.BillingCostSnapshot{CostBillingMode: "per_second", CostQuantity: 1},
		ReservedAmountMicrocredits: 100, Status: model.BillingStatusRunning,
	}).Error; err != nil {
		t.Fatal(err)
	}

	repo := &Repository{db: db}
	// The legacy duration-shaped entry point must still consume the quoted audio
	// amount; audio generation is no longer settled from measured output length.
	if err := repo.SettleBillingOrderWithAudioDuration("audio-order", "", 2_100); err != nil {
		t.Fatalf("SettleBillingOrderWithAudioDuration() error = %v", err)
	}
	if err := repo.SettleBillingOrder("audio-order", "provider-audio-task"); err != nil {
		t.Fatalf("idempotent audio settlement should retain provider request ID: %v", err)
	}
	var order model.BillingOrder
	if err := db.First(&order, "id = ?", "audio-order").Error; err != nil {
		t.Fatal(err)
	}
	if order.Status != model.BillingStatusSettled || order.Quantity != 1 || order.ActualAmountMicrocredits != 100 || order.RefundedAmountMicrocredits != 0 || order.CostQuantity != 1 || order.ProviderRequestID != "provider-audio-task" {
		t.Fatalf("settled audio order = %#v", order)
	}
	var account model.CreditAccount
	if err := db.First(&account, "user_id = ?", "user-audio").Error; err != nil {
		t.Fatal(err)
	}
	if account.AvailableMicrocredits != 1_000 || account.ReservedMicrocredits != 0 {
		t.Fatalf("settled audio account = %#v; want available=1000 reserved=0", account)
	}
}

func TestSettleAudioPerSecondOrderDoesNotRequireMeasuredDuration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-duration-required?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.CreditAccount{}, &model.BillingOrder{}, &model.CreditLedgerEntry{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.CreditAccount{UserID: "user-audio", ReservedMicrocredits: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "audio-order", UserID: "user-audio", IdempotencyKey: "audio-task", Capability: "audio", BillingMode: "per_second",
		UnitPriceMicrocredits: 100, MultiplierBasisPoints: 10_000, Quantity: 1, AmountMicrocredits: 100,
		ReservedAmountMicrocredits: 100, Status: model.BillingStatusRunning,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&Repository{db: db}).SettleBillingOrderWithAudioDuration("audio-order", "", 0); err != nil {
		t.Fatalf("quoted audio settlement should ignore duration: %v", err)
	}
}

func TestRefundAudioBillingOrderIsRejected(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-refund?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.CreditAccount{}, &model.BillingOrder{}, &model.CreditLedgerEntry{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.CreditAccount{UserID: "user-audio", AvailableMicrocredits: 900, ReservedMicrocredits: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "audio-refund-order", UserID: "user-audio", IdempotencyKey: "audio-refund-task", Capability: "audio", BillingMode: "per_second",
		AmountMicrocredits: 100, ReservedAmountMicrocredits: 100, Status: model.BillingStatusRunning,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&Repository{db: db}).RefundBillingOrder("audio-refund-order", "provider failed"); err == nil {
		t.Fatal("expected audio refund to be rejected")
	}
	var order model.BillingOrder
	if err := db.First(&order, "id = ?", "audio-refund-order").Error; err != nil {
		t.Fatal(err)
	}
	if order.Status != model.BillingStatusRunning {
		t.Fatalf("audio order status = %s, want running", order.Status)
	}
	var account model.CreditAccount
	if err := db.First(&account, "user_id = ?", "user-audio").Error; err != nil {
		t.Fatal(err)
	}
	if account.AvailableMicrocredits != 900 || account.ReservedMicrocredits != 100 {
		t.Fatalf("audio account changed after rejected refund: %#v", account)
	}
}

func TestLegacyAudioTaskWithoutCapabilityCannotRefundOrRestore(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-legacy-task?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Task{}, &model.CreditAccount{}, &model.BillingOrder{}, &model.CreditLedgerEntry{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Task{ID: "legacy-audio-task", Type: "canvas_generation", InputJSON: `{"mode":"audio"}`, Status: model.TaskStatusFailed}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.CreditAccount{UserID: "user-audio", AvailableMicrocredits: 900, ReservedMicrocredits: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "legacy-audio-order", UserID: "user-audio", TaskID: "legacy-audio-task", IdempotencyKey: "legacy-audio-task",
		BillingMode: "per_second", AmountMicrocredits: 100, ReservedAmountMicrocredits: 100, Status: model.BillingStatusRunning,
	}).Error; err != nil {
		t.Fatal(err)
	}
	repo := &Repository{db: db}
	if err := repo.RefundBillingOrder("legacy-audio-order", "provider failed"); err == nil {
		t.Fatal("expected legacy audio refund to be rejected")
	}
	if err := db.Model(&model.BillingOrder{}).Where("id = ?", "legacy-audio-order").Updates(map[string]any{"status": model.BillingStatusRefunded}).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.RestoreRefundedBillingOrder("legacy-audio-order", "provider-task"); err == nil {
		t.Fatal("expected legacy audio restore to be rejected")
	}
}

func TestBillingOrdersByTaskIDsReturnsNewestOrderDeterministically(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-billing-orders-by-task?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.BillingOrder{}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if err := db.Create(&model.BillingOrder{
		ID: "order-old", UserID: "user-audio", TaskID: "task-audio", IdempotencyKey: "old-order", CreatedAt: base,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "order-new", UserID: "user-audio", TaskID: "task-audio", IdempotencyKey: "new-order", CreatedAt: base.Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	orders, err := (&Repository{db: db}).BillingOrdersByTaskIDs("user-audio", []string{"task-audio"})
	if err != nil {
		t.Fatal(err)
	}
	if got := orders["task-audio"].ID; got != "order-new" {
		t.Fatalf("selected billing order = %q, want newest order", got)
	}
}

func TestAutoRefundableBillingOrdersExcludesLegacyInputAudio(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-auto-refund-input?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Task{}, &model.BillingOrder{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Task{
		ID: "legacy-input-audio-task", Type: "canvas_generation", InputJSON: `{"mode":"audio"}`, Status: model.TaskStatusFailed,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "legacy-input-audio-order", UserID: "user-audio", TaskID: "legacy-input-audio-task",
		IdempotencyKey: "legacy-input-audio-task", Status: model.BillingStatusReserved,
	}).Error; err != nil {
		t.Fatal(err)
	}
	orders, err := (&Repository{db: db}).AutoRefundableBillingOrders(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 0 {
		t.Fatalf("legacy input audio orders = %#v, want none", orders)
	}
}

func TestReleaseAudioBillingReservationRestoresBalanceAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-release?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.CreditAccount{}, &model.BillingOrder{}, &model.CreditLedgerEntry{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.CreditAccount{UserID: "user-audio", AvailableMicrocredits: 900, ReservedMicrocredits: 100}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "audio-release-order", UserID: "user-audio", IdempotencyKey: "audio-release-task", Capability: "audio", BillingMode: "per_second",
		AmountMicrocredits: 100, ReservedAmountMicrocredits: 100, Status: model.BillingStatusReserved,
	}).Error; err != nil {
		t.Fatal(err)
	}
	repo := &Repository{db: db}
	if err := repo.ReleaseBillingReservation("audio-release-order", "queued task cancelled"); err != nil {
		t.Fatalf("ReleaseBillingReservation() error = %v", err)
	}
	if err := repo.ReleaseBillingReservation("audio-release-order", "duplicate release"); err != nil {
		t.Fatalf("repeated ReleaseBillingReservation() error = %v", err)
	}
	var order model.BillingOrder
	if err := db.First(&order, "id = ?", "audio-release-order").Error; err != nil {
		t.Fatal(err)
	}
	if order.Status != model.BillingStatusReleased || order.ActualAmountMicrocredits != 0 || order.RefundedAmountMicrocredits != 0 {
		t.Fatalf("released audio order = %#v", order)
	}
	var account model.CreditAccount
	if err := db.First(&account, "user_id = ?", "user-audio").Error; err != nil {
		t.Fatal(err)
	}
	if account.AvailableMicrocredits != 1_000 || account.ReservedMicrocredits != 0 {
		t.Fatalf("released audio account = %#v; want available=1000 reserved=0", account)
	}
	var releaseCount, refundCount, consumeCount int64
	if err := db.Model(&model.CreditLedgerEntry{}).Where("billing_order_id = ? AND type = ?", order.ID, model.CreditLedgerRelease).Count(&releaseCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.CreditLedgerEntry{}).Where("billing_order_id = ? AND type = ?", order.ID, model.CreditLedgerRefund).Count(&refundCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.CreditLedgerEntry{}).Where("billing_order_id = ? AND type = ?", order.ID, model.CreditLedgerConsume).Count(&consumeCount).Error; err != nil {
		t.Fatal(err)
	}
	if releaseCount != 1 || refundCount != 0 || consumeCount != 0 {
		t.Fatalf("release ledger counts = release:%d refund:%d consume:%d", releaseCount, refundCount, consumeCount)
	}
	if err := repo.SettleBillingOrder(order.ID, "provider-request"); err == nil {
		t.Fatal("expected released audio order to reject settlement")
	}
}

func TestAutoRefundableBillingOrdersQuarantinesDetachedUnknownCapability(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-auto-refund-detached?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.BillingOrder{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "detached-legacy-order", UserID: "user-audio", Status: model.BillingStatusUncertain,
		Capability: " ", BillingMode: "per_second", AmountMicrocredits: 100,
	}).Error; err != nil {
		t.Fatal(err)
	}
	orders, err := (&Repository{db: db}).AutoRefundableBillingOrders(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 0 {
		t.Fatalf("detached unknown-capability orders = %#v, want quarantined", orders)
	}
}

func TestAudioBillingCannotRefundOrRestoreAfterRefundState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:finance-audio-refund-restore?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.CreditAccount{}, &model.BillingOrder{}, &model.CreditLedgerEntry{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.CreditAccount{UserID: "user-audio", AvailableMicrocredits: 1_000}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.BillingOrder{
		ID: "audio-refunded-order", UserID: "user-audio", IdempotencyKey: "audio-refunded-task", Capability: "audio", BillingMode: "per_second",
		AmountMicrocredits: 100, RefundedAmountMicrocredits: 100, Status: model.BillingStatusRefunded,
	}).Error; err != nil {
		t.Fatal(err)
	}
	repo := &Repository{db: db}
	if err := repo.RefundBillingOrder("audio-refunded-order", "duplicate refund"); err == nil {
		t.Fatal("expected already-refunded audio order to remain non-refundable")
	}
	if err := repo.RestoreRefundedBillingOrder("audio-refunded-order", "provider-task"); err == nil {
		t.Fatal("expected refunded audio order to remain non-restorable")
	}
	var order model.BillingOrder
	if err := db.First(&order, "id = ?", "audio-refunded-order").Error; err != nil {
		t.Fatal(err)
	}
	if order.Status != model.BillingStatusRefunded {
		t.Fatalf("audio order status = %s, want refunded", order.Status)
	}
	var account model.CreditAccount
	if err := db.First(&account, "user_id = ?", "user-audio").Error; err != nil {
		t.Fatal(err)
	}
	if account.AvailableMicrocredits != 1_000 || account.ReservedMicrocredits != 0 {
		t.Fatalf("audio account changed after blocked restore: %#v", account)
	}
	var ledgerCount int64
	if err := db.Model(&model.CreditLedgerEntry{}).Count(&ledgerCount).Error; err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 0 {
		t.Fatalf("audio blocked restore created %d ledger entries", ledgerCount)
	}
}
