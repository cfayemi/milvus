package inspector

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/types"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/syncutil"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

// NewTimeTickSyncInspector creates a new time tick sync inspector.
func NewTimeTickSyncInspector() TimeTickSyncInspector {
	inspector := &timeTickSyncInspectorImpl{
		taskNotifier: syncutil.NewAsyncTaskNotifier[struct{}](),
		syncNotifier: newSyncNotifier(),
		operators:    typeutil.NewConcurrentMap[string, TimeTickSyncOperator](),
	}
	go inspector.background()
	return inspector
}

type timeTickSyncInspectorImpl struct {
	taskNotifier *syncutil.AsyncTaskNotifier[struct{}]
	syncNotifier *syncNotifier
	operators    *typeutil.ConcurrentMap[string, TimeTickSyncOperator]
	wg           sync.WaitGroup
	working      typeutil.ConcurrentSet[string]
}

func (s *timeTickSyncInspectorImpl) TriggerSync(pChannelInfo types.PChannelInfo, persisted bool) {
	s.syncNotifier.AddAndNotify(pChannelInfo, persisted)
}

// GetOperator gets the operator by pchannel info.
func (s *timeTickSyncInspectorImpl) MustGetOperator(pChannelInfo types.PChannelInfo) TimeTickSyncOperator {
	operator, ok := s.operators.Get(pChannelInfo.Name)
	if !ok {
		panic("sync operator not found, critical bug in code")
	}
	return operator
}

// RegisterSyncOperator registers a sync operator.
func (s *timeTickSyncInspectorImpl) RegisterSyncOperator(operator TimeTickSyncOperator) {
	log.Info("RegisterSyncOperator", zap.String("channel", operator.Channel().Name))
	_, loaded := s.operators.GetOrInsert(operator.Channel().Name, operator)
	if loaded {
		panic("sync operator already exists, critical bug in code")
	}
}

// UnregisterSyncOperator unregisters a sync operator.
func (s *timeTickSyncInspectorImpl) UnregisterSyncOperator(operator TimeTickSyncOperator) {
	log.Info("UnregisterSyncOperator", zap.String("channel", operator.Channel().Name))
	_, loaded := s.operators.GetAndRemove(operator.Channel().Name)
	if !loaded {
		panic("sync operator not found, critical bug in code")
	}
}

// background executes the time tick sync inspector.
func (s *timeTickSyncInspectorImpl) background() {
	defer s.taskNotifier.Finish(struct{}{})

	interval := paramtable.Get().ProxyCfg.TimeTickInterval.GetAsDuration(time.Millisecond)
	ticker := time.NewTicker(interval)
	for {
		select {
		case <-s.taskNotifier.Context().Done():
			return
		case <-ticker.C:
			s.operators.Range(func(name string, _ TimeTickSyncOperator) bool {
				s.asyncSync(name, false)
				return true
			})
		case <-s.syncNotifier.WaitChan():
			signals := s.syncNotifier.Get()
			for pchannel, persisted := range signals {
				s.asyncSync(pchannel.Name, persisted)
			}
		}
	}
}

// asyncSync syncs the pchannel in a goroutine.
func (s *timeTickSyncInspectorImpl) asyncSync(pchannelName string, persisted bool) {
	if !s.working.Insert(pchannelName) {
		// Check if the sync operation of pchannel is working, if so, skip it.
		return
	}

	s.wg.Add(1)
	go func() {
		defer func() {
			s.wg.Done()
			s.working.Remove(pchannelName)
		}()
		if operator, ok := s.operators.Get(pchannelName); ok {
			operator.Sync(s.taskNotifier.Context(), persisted)
		}
	}()
}

func (s *timeTickSyncInspectorImpl) Close() {
	s.taskNotifier.Cancel()
	s.taskNotifier.BlockUntilFinish()
	s.wg.Wait()
}
