package optimus

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/sonm-io/core/blockchain"
	"github.com/sonm-io/core/insonmnia/benchmarks"
	"github.com/sonm-io/core/insonmnia/hardware"
	"github.com/sonm-io/core/proto"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	minNumOrders = sonm.MinNumBenchmarks
)

type optimizationInput struct {
	Orders  []*MarketOrder
	Devices *sonm.DevicesReply
	Plans   map[string]*sonm.AskPlan
}

// VictimPlans returns plans that can be removed to be replaced with another
// plans.
// This is useful to virtualize worker free devices, that are currently
// occupied by either nearly-to-expire or spot plans.
func (m *optimizationInput) VictimPlans() map[string]*sonm.AskPlan {
	victims := map[string]*sonm.AskPlan{}
	for id, plan := range m.Plans {
		// Currently we can remove spot orders without regret.
		if plan.GetDuration().Unwrap() == 0 {
			victims[id] = plan
		}
	}

	return victims
}

func (m *optimizationInput) FreeDevices() (*sonm.DevicesReply, error) {
	return m.freeDevices(map[string]*sonm.AskPlan{})
}

func (m *optimizationInput) VirtualFreeDevices() (*sonm.DevicesReply, error) {
	return m.freeDevices(m.VictimPlans())
}

func (m *optimizationInput) Price() *sonm.Price {
	var plans []*sonm.AskPlan
	for _, plan := range m.Plans {
		plans = append(plans, plan)
	}

	return sonm.SumPrice(plans)
}

func (m *optimizationInput) freeDevices(removalVictims map[string]*sonm.AskPlan) (*sonm.DevicesReply, error) {
	workerHardware := hardware.Hardware{
		CPU:     m.Devices.CPU,
		GPU:     m.Devices.GPUs,
		RAM:     m.Devices.RAM,
		Network: m.Devices.Network,
		Storage: m.Devices.Storage,
	}
	// All resources are free by default.
	freeResources := workerHardware.AskPlanResources()

	// Subtract plans except cancellation removalVictims. Doing so produces us a
	// new free(!) devices list.
	for id, plan := range m.Plans {
		if _, ok := removalVictims[id]; !ok {
			if err := freeResources.Sub(plan.Resources); err != nil {
				return nil, fmt.Errorf("failed to virtualize resource releasing: %v", err)
			}
		}
	}

	freeWorkerHardware, err := workerHardware.LimitTo(freeResources)
	if err != nil {
		return nil, fmt.Errorf("failed to limit virtual free hardware: %v", err)
	}

	return freeWorkerHardware.IntoProto(), nil
}

type Blacklist interface {
	Update(ctx context.Context) error
	IsAllowed(addr common.Address) bool
}

type workerEngine struct {
	cfg *workerConfig
	log *zap.SugaredLogger

	addr             common.Address
	masterAddr       common.Address
	blacklist        Blacklist
	market           blockchain.MarketAPI
	marketCache      *MarketCache
	worker           WorkerManagementClientExt
	benchmarkMapping benchmarks.Mapping

	tagger *Tagger
}

func newWorkerEngine(cfg *workerConfig, addr, masterAddr common.Address, blacklist Blacklist, worker sonm.WorkerManagementClient, market blockchain.MarketAPI, marketCache *MarketCache, benchmarkMapping benchmarks.Mapping, tagger *Tagger, log *zap.SugaredLogger) (*workerEngine, error) {
	m := &workerEngine{
		cfg: cfg,
		log: log.With(zap.Stringer("addr", addr)),

		addr:             addr,
		masterAddr:       masterAddr,
		blacklist:        blacklist,
		market:           market,
		marketCache:      marketCache,
		worker:           &workerManagementClientExt{worker},
		benchmarkMapping: benchmarkMapping,

		tagger: tagger,
	}

	return m, nil
}

func (m *workerEngine) OnRun() {
	m.log.Info("managing worker")
}

func (m *workerEngine) OnShutdown() {
	m.log.Info("stop managing worker")
}

func (m *workerEngine) Execute(ctx context.Context) {
	m.log.Info("optimization epoch started")

	if err := m.execute(ctx); err != nil {
		m.log.Warn(err.Error())
	}
}

func (m *workerEngine) execute(ctx context.Context) error {
	maintenance, err := m.worker.NextMaintenance(ctx, &sonm.Empty{})
	if err != nil {
		return fmt.Errorf("failed to get maintenance: %v", err)
	}
	if time.Since(maintenance.Unix()) >= 0 {
		return fmt.Errorf("worker is on the maintenance")
	}

	if err := m.blacklist.Update(ctx); err != nil {
		return fmt.Errorf("failed to update blacklist: %v", err)
	}

	input, err := m.optimizationInput(ctx)
	if err != nil {
		return err
	}

	m.log.Debugf("pulled %d orders from the marketplace", len(input.Orders))
	m.log.Debugw("pulled worker devices", zap.Any("devices", *input.Devices))
	m.log.Debugw("pulled worker plans", zap.Any("plans", input.Plans))

	removedPlans, err := m.tryRemoveUnsoldPlans(ctx, input.Plans)
	if err != nil {
		return err
	}

	if len(removedPlans) != 0 {
		return m.execute(ctx)
	}

	victimPlans := input.VictimPlans()
	m.log.Debugw("victim plans", zap.Any("plans", victimPlans))

	naturalFreeDevices, err := input.FreeDevices()
	if err != nil {
		return err
	}

	virtualFreeDevices, err := input.VirtualFreeDevices()
	if err != nil {
		return err
	}

	m.log.Debugw("virtualized worker natural free devices", zap.Any("devices", *naturalFreeDevices))
	m.log.Debugw("virtualized worker virtual free devices", zap.Any("devices", *virtualFreeDevices))

	// Here we append removal candidate's orders to "orders" from the
	// marketplace to be able to track their profitability.
	// Note, that this can return error when some victim plans did not place
	// their orders on the marketplace.
	// Either this can be temporary or worker's critical failure, network for
	// example.
	// The best we can do here is to return and try again in the next epoch.
	virtualFreeOrders, err := m.ordersForPlans(ctx, victimPlans)
	if err != nil {
		return fmt.Errorf("failed to collect orders for victim plans: %v", err)
	}

	// Extended orders set, with added currently executed orders.
	extOrders := append(append([]*MarketOrder{}, input.Orders...), virtualFreeOrders...)

	var naturalKnapsack, virtualKnapsack *Knapsack

	wg := errgroup.Group{}
	wg.Go(func() error {
		m.log.Info("optimizing using natural free devices")
		knapsack, err := m.optimize(input.Devices, naturalFreeDevices, input.Orders, m.log.With(zap.String("optimization", "natural")))
		if err != nil {
			return err
		}

		naturalKnapsack = knapsack
		return nil
	})
	wg.Go(func() error {
		m.log.Info("optimizing using virtual free devices")
		knapsack, err := m.optimize(input.Devices, virtualFreeDevices, extOrders, m.log.With(zap.String("optimization", "virtual")))
		if err != nil {
			return err
		}

		virtualKnapsack = knapsack
		return nil
	})
	if err := wg.Wait(); err != nil {
		return err
	}

	m.log.Infow("current worker price", zap.String("Σ USD/s", input.Price().GetPerSecond().ToPriceString()))
	m.log.Infow("optimizing using natural free devices done", zap.String("Σ USD/s", naturalKnapsack.Price().GetPerSecond().ToPriceString()), zap.Any("plans", naturalKnapsack.Plans()))
	m.log.Infow("optimizing using virtual free devices done", zap.String("Σ USD/s", virtualKnapsack.Price().GetPerSecond().ToPriceString()), zap.Any("plans", virtualKnapsack.Plans()))

	if m.cfg.DryRun {
		return fmt.Errorf("further worker management has been interrupted: dry-run mode is active")
	}

	// Compare total USD/s before and after. Remove some plans if the diff is
	// more than the threshold.
	priceThreshold := m.cfg.PriceThreshold.GetPerSecond()
	priceDiff := new(big.Int).Sub(virtualKnapsack.Price().GetPerSecond().Unwrap(), input.Price().GetPerSecond().Unwrap())
	swingTime := new(big.Int).Sub(priceDiff, priceThreshold.Unwrap()).Sign() >= 0

	var winners []*sonm.AskPlan
	if swingTime {
		m.log.Info("using replacement strategy")

		create, remove, ignore := m.splitPlans(input.Plans, virtualKnapsack.Plans())
		m.log.Infow("ignoring already existing plans", zap.Any("plans", ignore))
		m.log.Infow("removing plans", zap.Any("plans", remove))

		victims := make([]string, 0, len(remove))
		for _, plan := range remove {
			victims = append(victims, plan.ID)
		}
		if err := m.worker.RemoveAskPlans(ctx, victims); err != nil {
			return err
		}

		winners = create
	} else {
		m.log.Info("using appending strategy")
		winners = naturalKnapsack.Plans()
	}

	if len(winners) == 0 {
		return fmt.Errorf("no plans found")
	}

	for _, plan := range winners {
		// Extract the order ID for whose the selling plan is created.
		orderID := plan.GetOrderID()

		// Then we need to clean this, because otherwise worker rejects such request.
		plan.OrderID = nil
		plan.Identity = m.cfg.Identity
		plan.Tag = m.tagger.Tag()

		id, err := m.worker.CreateAskPlan(ctx, plan)
		if err != nil {
			m.log.Warnw("failed to create sell plan", zap.Any("plan", *plan), zap.Error(err))
			continue
		}

		m.log.Infof("created sell plan %s for %s order", id.Id, orderID.String())
	}

	return nil
}

func (m *workerEngine) splitPlans(plans map[string]*sonm.AskPlan, candidates []*sonm.AskPlan) (create, remove, ignore []*sonm.AskPlan) {
	orders := map[string]struct{}{}
	for _, plan := range plans {
		orders[plan.GetOrderID().Unwrap().String()] = struct{}{}
	}

	newOrders := map[string]struct{}{}
	for _, plan := range candidates {
		newOrders[plan.GetOrderID().Unwrap().String()] = struct{}{}
	}

	for _, plan := range candidates {
		if _, ok := orders[plan.GetOrderID().Unwrap().String()]; ok {
			ignore = append(ignore, plan)
		} else {
			create = append(create, plan)
		}
	}

	for _, plan := range plans {
		if _, ok := newOrders[plan.GetOrderID().Unwrap().String()]; !ok {
			remove = append(remove, plan)
		}
	}

	return create, remove, ignore
}

func (m *workerEngine) optimizationInput(ctx context.Context) (*optimizationInput, error) {
	input := &optimizationInput{}

	ctx, cancel := context.WithTimeout(ctx, m.cfg.PreludeTimeout)
	defer cancel()

	// Concurrently fetch all required inputs, such as market orders, worker
	// devices and plans.
	wg, ctx := errgroup.WithContext(ctx)
	wg.Go(func() error {
		orders, err := m.marketCache.ActiveOrders(ctx)
		if err != nil {
			return fmt.Errorf("failed to pull market orders: %v", err)
		}
		if len(orders) == 0 {
			return fmt.Errorf("not enough orders to perform optimization")
		}

		input.Orders = orders
		return nil
	})
	wg.Go(func() error {
		devices, err := m.worker.Devices(ctx, &sonm.Empty{})
		if err != nil {
			return fmt.Errorf("failed to pull worker devices: %v", err)
		}

		input.Devices = devices
		return nil
	})
	wg.Go(func() error {
		plans, err := m.worker.AskPlans(ctx, &sonm.Empty{})
		if err != nil {
			return fmt.Errorf("failed to pull worker plans: %v", err)
		}

		input.Plans = plans.GetAskPlans()
		return nil
	})

	if err := wg.Wait(); err != nil {
		return nil, err
	}

	return input, nil
}

func (m *workerEngine) tryRemoveUnsoldPlans(ctx context.Context, plans map[string]*sonm.AskPlan) ([]string, error) {
	victims := make([]string, 0, len(plans))
	for id, plan := range plans {
		if plan.UnsoldDuration() >= m.cfg.StaleThreshold {
			victims = append(victims, id)
		}
	}

	if len(victims) == 0 {
		m.log.Info("no unsold plans found")
		return victims, nil
	}

	m.log.Infow("removing unsold plans", zap.Duration("threshold", m.cfg.StaleThreshold), zap.Any("plans", victims))
	if err := m.worker.RemoveAskPlans(ctx, victims); err != nil {
		return nil, fmt.Errorf("failed to remove some unsold plans: %v", err)
	}

	return victims, nil
}

func (m *workerEngine) ordersForPlans(ctx context.Context, plans map[string]*sonm.AskPlan) ([]*MarketOrder, error) {
	var orders []*MarketOrder

	mu := sync.Mutex{}
	wg, ctx := errgroup.WithContext(ctx)

	for id, plan := range plans {
		id := id
		plan := plan

		wg.Go(func() error {
			order, err := m.market.GetOrderInfo(ctx, plan.OrderID.Unwrap())
			if err != nil {
				return fmt.Errorf("failed to get order `%s` for `%s`: %v", plan.OrderID.Unwrap().String(), id, err)
			}

			mu.Lock()
			defer mu.Unlock()
			orders = append(orders, &MarketOrder{
				Order:     order,
				CreatedTS: sonm.CurrentTimestamp(),
			})

			return nil
		})
	}

	if err := wg.Wait(); err != nil {
		return nil, err
	}

	return orders, nil
}

func (m *workerEngine) optimize(devices, freeDevices *sonm.DevicesReply, orders []*MarketOrder, log *zap.SugaredLogger) (*Knapsack, error) {
	deviceManager, err := newDeviceManager(devices, freeDevices, m.benchmarkMapping)
	if err != nil {
		return nil, fmt.Errorf("failed to construct device manager: %v", err)
	}

	matchedOrders := m.matchingOrders(deviceManager, devices, orders)
	log.Infof("found %d/%d matching orders", len(matchedOrders), len(orders))

	if len(matchedOrders) == 0 {
		log.Infof("no matching orders found")
		return NewKnapsack(deviceManager), nil
	}

	now := time.Now()
	knapsack := NewKnapsack(deviceManager)
	if err := m.optimizationMethod(orders, matchedOrders, log).Optimize(knapsack, matchedOrders); err != nil {
		return nil, err
	}

	log.Infof("optimized %d orders in %s", len(matchedOrders), time.Since(now))

	return knapsack, nil
}

// MatchingOrders filters the given orders to have only orders that are subset
// of ours.
func (m *workerEngine) matchingOrders(deviceManager *DeviceManager, devices *sonm.DevicesReply, orders []*MarketOrder) []*MarketOrder {
	matchedOrders := make([]*MarketOrder, 0, len(orders))

	filter := FittingFunc{
		Filters: m.filters(deviceManager, devices),
	}

	for _, order := range orders {
		if filter.Filter(order.GetOrder()) {
			matchedOrders = append(matchedOrders, order)
		}
	}

	return matchedOrders
}

func (m *workerEngine) filters(deviceManager *DeviceManager, devices *sonm.DevicesReply) []func(order *sonm.Order) bool {
	return []func(order *sonm.Order) bool{
		func(order *sonm.Order) bool {
			return order.OrderType == sonm.OrderType_BID
		},
		func(order *sonm.Order) bool {
			switch m.cfg.OrderPolicy {
			case PolicySpotOnly:
				return order.GetDuration() == 0
			}
			return false
		},
		func(order *sonm.Order) bool {
			return m.blacklist.IsAllowed(order.GetAuthorID().Unwrap())
		},
		func(order *sonm.Order) bool {
			return devices.GetNetwork().GetNetFlags().ConverseImplication(order.GetNetflags())
		},
		func(order *sonm.Order) bool {
			counterpartyID := order.CounterpartyID.Unwrap()
			return counterpartyID == common.Address{} || counterpartyID == m.addr || counterpartyID == m.masterAddr
		},
		func(order *sonm.Order) bool {
			return deviceManager.Contains(*order.Benchmarks, *order.Netflags)
		},
	}
}

func (m *workerEngine) optimizationMethod(orders, matchedOrders []*MarketOrder, log *zap.SugaredLogger) OptimizationMethod {
	return m.cfg.Optimization.Model.Create(orders, matchedOrders, log)
}

type OptimizationMethodFactory interface {
	Config() interface{}
	Create(orders, matchedOrders []*MarketOrder, log *zap.SugaredLogger) OptimizationMethod
}

type defaultOptimizationMethodFactory struct{}

func (m *defaultOptimizationMethodFactory) Config() interface{} {
	return m
}

func (m *defaultOptimizationMethodFactory) Create(orders, matchedOrders []*MarketOrder, log *zap.SugaredLogger) OptimizationMethod {
	if len(matchedOrders) < 128 {
		return &BranchBoundModel{
			Log: log.With(zap.String("model", "BBM")),
		}
	}

	return &BatchModel{
		Methods: []OptimizationMethod{
			&GreedyLinearRegressionModel{
				orders: orders,
				regression: &regressionClassifier{
					model: &SCAKKTModel{
						MaxIterations: 1e7,
						Log:           log,
					},
				},
				exhaustionLimit: 128,
				log:             log.With(zap.String("model", "LLS")),
			},
			&GeneticModel{
				NewGenomeLab:   NewPackedOrdersNewGenome,
				PopulationSize: 256,
				MaxGenerations: 128,
				MaxAge:         5 * time.Minute,
				Log:            log.With(zap.String("model", "GMP")),
			},
			&GeneticModel{
				NewGenomeLab:   NewDecisionOrdersNewGenome,
				PopulationSize: 512,
				MaxGenerations: 64,
				MaxAge:         5 * time.Minute,
				Log:            log.With(zap.String("model", "GMD")),
			},
		},
		Log: log,
	}
}

func optimizationFactory(ty string) OptimizationMethodFactory {
	switch ty {
	case "batch":
		return &BatchModelFactory{}
	case "greedy":
		return &GreedyLinearRegressionModelFactory{}
	case "genetic":
		return &GeneticModelFactory{}
	case "branch_bound":
		return &BranchBoundModelFactory{}
	default:
		return nil
	}
}

type optimizationMethodFactory struct {
	OptimizationMethodFactory
}

func (m *optimizationMethodFactory) MarshalYAML() (interface{}, error) {
	return m.Config(), nil
}

func (m *optimizationMethodFactory) UnmarshalYAML(unmarshal func(interface{}) error) error {
	ty, err := typeofInterface(unmarshal)
	if err != nil {
		return err
	}

	factory := optimizationFactory(ty)
	if factory == nil {
		return fmt.Errorf("unknown optimization model: %s", ty)
	}

	cfg := factory.Config()
	if err := unmarshal(cfg); err != nil {
		return err
	}

	m.OptimizationMethodFactory = factory

	return nil
}

type OptimizationMethod interface {
	Optimize(knapsack *Knapsack, orders []*MarketOrder) error
}

type FittingFunc struct {
	Filters []func(order *sonm.Order) bool
}

func (m *FittingFunc) Filter(order *sonm.Order) bool {
	for _, filter := range m.Filters {
		if !filter(order) {
			return false
		}
	}

	return true
}

type Knapsack struct {
	manager *DeviceManager
	plans   []*sonm.AskPlan
}

func NewKnapsack(deviceManager *DeviceManager) *Knapsack {
	return &Knapsack{
		manager: deviceManager,
	}
}

func (m *Knapsack) Clone() *Knapsack {
	plans := make([]*sonm.AskPlan, len(m.plans))
	copy(plans, m.plans)

	return &Knapsack{
		manager: m.manager.Clone(),
		plans:   plans,
	}
}

func (m *Knapsack) Put(order *sonm.Order) error {
	resources, err := m.manager.Consume(*order.GetBenchmarks(), *order.GetNetflags())
	if err != nil {
		return err
	}

	resources.Network.NetFlags = order.GetNetflags()

	m.plans = append(m.plans, &sonm.AskPlan{
		OrderID:   order.GetId(),
		Price:     &sonm.Price{PerSecond: order.Price},
		Duration:  &sonm.Duration{Nanoseconds: 1e9 * int64(order.Duration)},
		Resources: resources,
	})

	return nil
}

func (m *Knapsack) Price() *sonm.Price {
	return sonm.SumPrice(m.plans)
}

func (m *Knapsack) PPSf64() float64 {
	return float64(m.Price().GetPerSecond().Unwrap().Uint64()) * 1e-18
}

func (m *Knapsack) Plans() []*sonm.AskPlan {
	return m.plans
}
