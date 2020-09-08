package circuitbreaker

import (
	"fmt"
	"strings"
	"sync"

	"github.com/alibaba/sentinel-golang/logging"
	"github.com/alibaba/sentinel-golang/util"
	"github.com/pkg/errors"
)

type CircuitBreakerGenFunc func(r *Rule, reuseStat interface{}) (CircuitBreaker, error)

var (
	cbGenFuncMap = make(map[Strategy]CircuitBreakerGenFunc)

	breakerRules = make(map[string][]*Rule)
	breakers     = make(map[string][]CircuitBreaker)
	updateMux    = &sync.RWMutex{}

	stateChangeListeners = make([]StateChangeListener, 0)
)

func init() {
	cbGenFuncMap[SlowRequestRatio] = func(r *Rule, reuseStat interface{}) (CircuitBreaker, error) {
		if r == nil {
			return nil, errors.New("nil rule")
		}
		if reuseStat == nil {
			return newSlowRtCircuitBreaker(r)
		}
		stat, ok := reuseStat.(*slowRequestLeapArray)
		if !ok || stat == nil {
			logging.Warnf("Expect to generate circuit breaker with reuse statistic, but fail to do type assertion, expect:*slowRequestLeapArray, in fact: %+v", stat)
			return newSlowRtCircuitBreaker(r)
		}
		return newSlowRtCircuitBreakerWithStat(r, stat), nil
	}

	cbGenFuncMap[ErrorRatio] = func(r *Rule, reuseStat interface{}) (CircuitBreaker, error) {
		if r == nil {
			return nil, errors.New("nil rule")
		}
		if reuseStat == nil {
			return newErrorRatioCircuitBreaker(r)
		}
		stat, ok := reuseStat.(*errorCounterLeapArray)
		if !ok || stat == nil {
			logging.Warnf("Expect to generate circuit breaker with reuse statistic, but fail to do type assertion, expect:*errorCounterLeapArray, in fact: %+v", stat)
			return newErrorRatioCircuitBreaker(r)
		}
		return newErrorRatioCircuitBreakerWithStat(r, stat), nil
	}

	cbGenFuncMap[ErrorCount] = func(r *Rule, reuseStat interface{}) (CircuitBreaker, error) {
		if r == nil {
			return nil, errors.New("nil rule")
		}
		if reuseStat == nil {
			return newErrorCountCircuitBreaker(r)
		}
		stat, ok := reuseStat.(*errorCounterLeapArray)
		if !ok || stat == nil {
			logging.Warnf("Expect to generate circuit breaker with reuse statistic, but fail to do type assertion, expect:*errorCounterLeapArray, in fact: %+v", stat)
			return newErrorCountCircuitBreaker(r)
		}
		return newErrorCountCircuitBreakerWithStat(r, stat), nil
	}
}

func GetResRules(resource string) []*Rule {
	updateMux.RLock()
	ret, ok := breakerRules[resource]
	updateMux.RUnlock()
	if !ok {
		ret = make([]*Rule, 0)
	}
	return ret
}

// ClearRules clear all the previous rules.
func ClearRules() error {
	_, err := LoadRules(nil)
	return err
}

// LoadRules replaces old rules with the given circuit breaking rules.
//
// return value:
//
// bool: was designed to indicate whether the internal map has been changed
// error: was designed to indicate whether occurs the error.
func LoadRules(rules []*Rule) (bool, error) {
	// TODO in order to avoid invalid update, should check consistent with last update rules
	err := onRuleUpdate(rules)
	return true, err
}

func getResBreakers(resource string) []CircuitBreaker {
	ret := make([]CircuitBreaker, 0)
	updateMux.RLock()
	resCBs := breakers[resource]
	updateMux.RUnlock()
	if len(resCBs) == 0 {
		return ret
	}
	ret = append(ret, resCBs...)
	return ret
}

func calculateReuseIndexFor(r *Rule, oldResCbs []CircuitBreaker) (equalIdx, reuseStatIdx int) {
	// the index of equivalent rule in old circuit breaker slice
	equalIdx = -1
	// the index of statistic reusable rule in old circuit breaker slice
	reuseStatIdx = -1

	for idx, oldTc := range oldResCbs {
		oldRule := oldTc.BoundRule()
		if oldRule.isEqualsTo(r) {
			// break if there is equivalent rule
			equalIdx = idx
			break
		}
		// find the index of first StatReusable rule
		if !oldRule.isStatReusable(r) {
			continue
		}
		if reuseStatIdx >= 0 {
			// had find reuse rule.
			continue
		}
		reuseStatIdx = idx
	}
	return equalIdx, reuseStatIdx
}

func insertCbToCbMap(cb CircuitBreaker, res string, m map[string][]CircuitBreaker) {
	cbsOfRes, exists := m[res]
	if !exists {
		cbsOfRes = make([]CircuitBreaker, 0, 1)
		m[res] = append(cbsOfRes, cb)
	} else {
		m[res] = append(cbsOfRes, cb)
	}
}

// Concurrent safe to update rules
func onRuleUpdate(rules []*Rule) (err error) {
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("%+v", r)
			}
		}
	}()

	newBreakerRules := make(map[string][]*Rule)
	for _, rule := range rules {
		if rule == nil {
			continue
		}
		if err := IsValid(rule); err != nil {
			logging.Warnf("Ignoring invalid circuit breaking rule when loading new rules, rule: %+v, reason: %s", rule, err.Error())
			continue
		}

		classification := rule.ResourceName()
		ruleSet, ok := newBreakerRules[classification]
		if !ok {
			ruleSet = make([]*Rule, 0, 1)
		}
		ruleSet = append(ruleSet, rule)
		newBreakerRules[classification] = ruleSet
	}

	newBreakers := make(map[string][]CircuitBreaker)
	// in order to avoid growing, build newBreakers in advance
	for res, rules := range newBreakerRules {
		newBreakers[res] = make([]CircuitBreaker, 0, len(rules))
	}

	start := util.CurrentTimeNano()
	updateMux.Lock()
	defer func() {
		updateMux.Unlock()
		if r := recover(); r != nil {
			return
		}
		logging.Debugf("Updating circuit breaker rule spends %d ns.", util.CurrentTimeNano()-start)
		logRuleUpdate(newBreakerRules)
	}()

	for res, resRules := range newBreakerRules {
		emptyCircuitBreakerList := make([]CircuitBreaker, 0, 0)
		for _, r := range resRules {
			oldResCbs := breakers[res]
			if oldResCbs == nil {
				oldResCbs = emptyCircuitBreakerList
			}
			if err := isAppend(oldResCbs, r); err != nil {
				logging.Warn(err)
				continue
			}
			equalIdx, reuseStatIdx := calculateReuseIndexFor(r, oldResCbs)

			// First check equals scenario
			if equalIdx >= 0 {
				// reuse the old cb
				equalOldCb := oldResCbs[equalIdx]
				insertCbToCbMap(equalOldCb, res, newBreakers)
				// remove old cb from oldResCbs
				breakers[res] = append(oldResCbs[:equalIdx], oldResCbs[equalIdx+1:]...)
				continue
			}

			cb, err := buildCircuitBreaker(reuseStatIdx, r, oldResCbs)
			if err != nil {
				logging.Warnf("BuildCircuitBreaker error: %v", err)
				continue
			}

			if reuseStatIdx >= 0 {
				breakers[res] = append(oldResCbs[:reuseStatIdx], oldResCbs[reuseStatIdx+1:]...)
			}
			insertCbToCbMap(cb, res, newBreakers)
		}
	}

	breakerRules = newBreakerRules
	breakers = newBreakers
	return nil
}

func isAppend(rulesOfRes []CircuitBreaker, rule *Rule) error {
	if rule != nil {
		for _, v := range rulesOfRes {
			if v.BoundRule().Id == rule.Id {
				return errors.New(fmt.Sprintf("Rule id:%s duplicate , current rule cannot be loaded", rule.Id))
			}
		}
	}
	return nil
}

func AppendRule(r *Rule) error {
	if err := IsValid(r); err != nil {
		return errors.New(fmt.Sprintf("Failed to append circuitbreaker rule, rule: %s, reason: %s", r.String(), err.Error()))
	}
	updateMux.Lock()
	defer func() {
		updateMux.Unlock()
		if r := recover(); r != nil {
			return
		}
	}()
	res := r.ResourceName()
	oldResCbs := breakers[res]
	if oldResCbs == nil {
		oldResCbs = make([]CircuitBreaker, 0, 0)
	}
	if err := isAppend(oldResCbs, r); err != nil {
		return err
	}
	equalIdx, _ := calculateReuseIndexFor(r, oldResCbs)

	if equalIdx >= 0 {
		return errors.New("The current appended rule already exists.")
	}

	cb, err := buildCircuitBreaker(-1, r, oldResCbs)
	if err != nil {
		return err
	}

	insertCbToCbMap(cb, res, breakers)
	breakerRules[res] = append(breakerRules[res], r)
	return nil
}

func buildCircuitBreaker(reuseStatIdx int, r *Rule, oldResCbs []CircuitBreaker) (CircuitBreaker, error) {
	generator := cbGenFuncMap[r.Strategy]
	if generator == nil {
		return nil, errors.New(fmt.Sprintf("Ignoring the rule due to unsupported circuit breaking strategy: %v", r))
	}
	var cb CircuitBreaker
	var e error
	if reuseStatIdx >= 0 {
		cb, e = generator(r, oldResCbs[reuseStatIdx].BoundStat())
	} else {
		cb, e = generator(r, nil)
	}
	if cb == nil || e != nil {
		return nil, errors.New(fmt.Sprintf("Ignoring the rule due to bad generated circuit breaker, r: %s, err: %+v", r.String(), e))
	}
	return cb, nil
}

func UpdateRule(id string, r *Rule) error {
	if err := IsValid(r); err != nil {
		return errors.New(fmt.Sprintf("Failed to update circuitbreaker rule, rule: %s, reason: %s", r.String(), err.Error()))
	}
	if id == "" {
		return errors.New("invalid Id")
	}
	updateMux.Lock()
	defer func() {
		updateMux.Unlock()
		if r := recover(); r != nil {
			return
		}
	}()
	res := r.ResourceName()
	oldResCbs := breakers[res]
	if oldResCbs == nil {
		return errors.New("Update failed, the current circuitBreaker resource to be updated does not exist.")
	}
	oldRules := breakerRules[res]
	if oldRules == nil {
		return errors.New("Update failed, the current rule resource to be updated does not exist.")
	}

	if err := updateSpecifiedRule(id, r, oldRules); err != nil {
		return err
	}

	if err := updateSpecifiedCircuitBreaker(id, r, oldResCbs); err != nil {
		return err
	}
	return nil
}

func updateSpecifiedCircuitBreaker(updateRuleId string, r *Rule, oldResCbs []CircuitBreaker) error {
	res := r.ResourceName()
	for index, oldResCb := range oldResCbs {
		if updateRuleId == oldResCb.BoundRule().Id {
			var (
				cb  CircuitBreaker
				err error
			)
			if oldResCb.BoundRule().isStatReusable(r) {
				cb, err = buildCircuitBreaker(index, r, oldResCbs)
			} else {
				cb, err = buildCircuitBreaker(-1, r, oldResCbs)
			}

			if err != nil {
				return errors.New(fmt.Sprintf("Build circuitBreaker error: %v", err))
			}
			oldResCbs[index] = cb
			breakers[res] = oldResCbs
			return nil
		}
	}
	return errors.New(fmt.Sprintf("Rule to be updated was not found,id:%s", updateRuleId))
}

func updateSpecifiedRule(updateRuleId string, r *Rule, oldRules []*Rule) error {
	for index, oldRule := range oldRules {
		if r.isEqualsTo(oldRule) {
			return errors.New("The rule to be updated already exists.")
		}

		if updateRuleId == oldRule.Id {
			oldRules[index] = r
			return nil
		}
	}
	return errors.New(fmt.Sprintf("Rule to be updated was not found,id:%s", updateRuleId))
}

func rulesFrom(rm map[string][]*Rule) []*Rule {
	rules := make([]*Rule, 0)
	if len(rm) == 0 {
		return rules
	}
	for _, rs := range rm {
		if len(rs) == 0 {
			continue
		}
		for _, r := range rs {
			if r != nil {
				rules = append(rules, r)
			}
		}
	}
	return rules
}

func logRuleUpdate(rules map[string][]*Rule) {
	sb := strings.Builder{}
	sb.WriteString("Circuit breaking rules loaded: [")

	for _, r := range rulesFrom(rules) {
		sb.WriteString(r.String() + ",")
	}
	sb.WriteString("]")
	logging.Info(sb.String())
}

// Note: this function is not thread-safe.
func RegisterStateChangeListeners(listeners ...StateChangeListener) {
	if len(listeners) == 0 {
		return
	}

	stateChangeListeners = append(stateChangeListeners, listeners...)
}

// ClearStateChangeListeners will clear the all StateChangeListener
// Note: this function is not thread-safe.
func ClearStateChangeListeners() {
	stateChangeListeners = make([]StateChangeListener, 0)
}

// SetCircuitBreakerGenerator sets the circuit breaker generator for the given strategy.
// Note that modifying the generator of default strategies is not allowed.
func SetCircuitBreakerGenerator(s Strategy, generator CircuitBreakerGenFunc) error {
	if generator == nil {
		return errors.New("nil generator")
	}
	if s >= SlowRequestRatio && s <= ErrorCount {
		return errors.New("not allowed to replace the generator for default circuit breaking strategies")
	}
	updateMux.Lock()
	defer updateMux.Unlock()

	cbGenFuncMap[s] = generator
	return nil
}

func RemoveCircuitBreakerGenerator(s Strategy) error {
	if s >= SlowRequestRatio && s <= ErrorCount {
		return errors.New("not allowed to replace the generator for default circuit breaking strategies")
	}
	updateMux.Lock()
	defer updateMux.Unlock()

	delete(cbGenFuncMap, s)
	return nil
}

func IsValid(r *Rule) error {
	if len(r.Resource) == 0 {
		return errors.New("empty resource name")
	}
	if int(r.Strategy) < int(SlowRequestRatio) || int(r.Strategy) > int(ErrorCount) {
		return errors.New("invalid Strategy")
	}
	if r.StatIntervalMs <= 0 {
		return errors.New("invalid StatIntervalMs")
	}
	if r.RetryTimeoutMs <= 0 {
		return errors.New("invalid RetryTimeoutMs")
	}
	if r.Threshold < 0.0 {
		return errors.New("invalid Threshold")
	}
	if r.Strategy == SlowRequestRatio && r.Threshold > 1.0 {
		return errors.New("invalid slow request ratio threshold (valid range: [0.0, 1.0])")
	}
	if r.Strategy == ErrorRatio && r.Threshold > 1.0 {
		return errors.New("invalid error ratio threshold (valid range: [0.0, 1.0])")
	}
	return nil
}
