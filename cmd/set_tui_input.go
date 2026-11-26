package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tui "github.com/grindlemire/go-tui"
	"github.com/grokbuildx/gbx/internal/locale"
	"github.com/grokbuildx/gbx/internal/oauthproxy"
	"github.com/grokbuildx/gbx/internal/provider"
)

// handleModelsDevPickerKey handles a key press while the models.dev overlay is
// open. Enter applies the selected provider and closes the overlay; esc/ctrl+c
// cancel; ↑↓ move the cursor; any other printable key filters the list.
func (m *AdvancedConfigModel) handleModelsDevPickerKey(ke tui.KeyEvent) {
	if ke.Key == tui.KeyEscape || ke.Mod == tui.ModCtrl && ke.Rune == 'c' {
		m.closeModelsDevPicker()
		return
	}
	switch ke.Key {
	case tui.KeyPageUp, tui.KeyPageDown:
		delta := m.pickerHeight(true)
		if ke.Key == tui.KeyPageUp {
			delta = -delta
		}
		m.modelsDevCursor = min(max(m.modelsDevCursor+delta, 0), max(len(m.modelsDevFiltered)-1, 0))
		m.keepPickersVisible()
	case tui.KeyUp:
		if m.modelsDevCursor > 0 {
			m.modelsDevCursor--
		}
		if m.modelsDevCursor < m.modelsDevWindow {
			m.modelsDevWindow = m.modelsDevCursor
		}
	case tui.KeyDown:
		if m.modelsDevCursor < len(m.modelsDevFiltered)-1 {
			m.modelsDevCursor++
		}
		if m.modelsDevCursor >= m.modelsDevWindow+m.pickerHeight(true) {
			m.modelsDevWindow = m.modelsDevCursor - m.pickerHeight(true) + 1
		}
	case tui.KeyEnter:
		if len(m.modelsDevFiltered) > 0 && m.modelsDevCursor >= 0 && m.modelsDevCursor < len(m.modelsDevFiltered) {
			m.applyModelsDevProvider(m.modelsDevFiltered[m.modelsDevCursor])
			m.closeModelsDevPicker()
		}
	case tui.KeyBackspace:
		t := m.modelsDevText.Get()
		if t != "" {
			m.modelsDevText.Set(removeLastRune(t))
		}
		m.updateModelsDevFilter()
	default:
		if ke.IsRune() && ke.Rune != 0 {
			m.modelsDevText.Set(m.modelsDevText.Get() + string(ke.Rune))
			m.updateModelsDevFilter()
		}
	}
}

func (m *AdvancedConfigModel) handleDefaultModelPickerKey(ke tui.KeyEvent) {
	if ke.Key == tui.KeyEscape || ke.Mod == tui.ModCtrl && ke.Rune == 'c' {
		m.closeDefaultModelPicker()
		return
	}
	if m.defaultModelText == nil {
		m.defaultModelText = tui.NewState("")
	}
	switch ke.Key {
	case tui.KeyPageUp, tui.KeyPageDown:
		delta := m.pickerHeight(true)
		if ke.Key == tui.KeyPageUp {
			delta = -delta
		}
		m.defaultModelCursor = min(max(m.defaultModelCursor+delta, 0), max(len(m.defaultModelFiltered)-1, 0))
		m.keepPickersVisible()
	case tui.KeyUp:
		if m.defaultModelCursor > 0 {
			m.defaultModelCursor--
		}
		m.keepPickersVisible()
	case tui.KeyDown:
		if m.defaultModelCursor < len(m.defaultModelFiltered)-1 {
			m.defaultModelCursor++
		}
		m.keepPickersVisible()
	case tui.KeyTab:
		if len(m.defaultModelFiltered) > 0 && m.defaultModelCursor >= 0 && m.defaultModelCursor < len(m.defaultModelFiltered) {
			if choice := m.defaultModelFiltered[m.defaultModelCursor]; choice != "" {
				m.togglePoolModelOneM(choice)
			}
		}
	case tui.KeyEnter:
		if len(m.defaultModelFiltered) > 0 && m.defaultModelCursor >= 0 && m.defaultModelCursor < len(m.defaultModelFiltered) {
			m.applyDefaultModelChoice(m.defaultModelFiltered[m.defaultModelCursor])
			m.closeDefaultModelPicker()
		}
	case tui.KeyBackspace:
		t := m.defaultModelText.Get()
		if t != "" {
			m.defaultModelText.Set(removeLastRune(t))
		}
		m.updateDefaultModelFilter()
	default:
		if ke.IsRune() && ke.Rune != 0 {
			m.defaultModelText.Set(m.defaultModelText.Get() + string(ke.Rune))
			m.updateDefaultModelFilter()
		}
	}
}

// activateRow fires the action for a button row on click or Enter: Auto
// Configure starts the connection check, Check starts the
// per-model probes. Async work is delivered through the model's channels and
// consumed on the main loop by the Watchers below.
func (m *AdvancedConfigModel) activateRow(kind configRowKind) {
	switch kind {
	case rowDefaultModel:
		m.openDefaultModelPicker()
	case rowProvider:
		m.openModelsDevPicker()
		fetchModelsDevAsync(m.lifecycleCtx(), m.mdDone)
	case rowSource:
		m.switchSource(m.otherSource())
	case rowTest:
		// Start detection with the current input values (OAuth uses the session
		// runtime endpoint/key already injected by configureOAuthRuntime).
		manualModels := ""
		preferManual := false
		if !m.usesOAuth() {
			m.p.Endpoint = m.urlText.Get()
			m.p.APIKey = m.keyText.Get()
			m.live().probeEndpoint = m.p.Endpoint
			m.live().probeAPIKey = m.p.APIKey
			if m.source == sourceCustom && m.modelsText != nil {
				manualModels = m.modelsText.Get()
				preferManual = m.live().manualModelsEdited
				m.live().probeModels = manualModels
			}
			// The inputs being detected become the new baseline only if the
			// detection succeeds; until then keep the dirty state honest.
			m.refreshConnectionDirty()
		}
		m.urlFocused = false
		m.keyFocused = false
		m.modelsFocused = false
		m.live().detectionError = nil
		// AutoClaw needs no network model detection: its managed Chat route has a
		// fixed catalog, so a generic /models probe would either hit the wrong
		// wire protocol or overwrite the fixed runtime metadata. Deliver the
		// built-in catalog through the regular detection-success path instead.
		if provider.IsAutoClawType(m.p.Type) {
			m.live().probeEndpoint = m.p.Endpoint
			m.live().probeAPIKey = m.keyText.Get()
			m.live().detecting = true
			m.live().detectProgress = 5
			m.live().detectFrame = 0
			probeEndpoint := m.live().probeEndpoint
			probeAPIKey := m.live().probeAPIKey
			providerType := m.p.Type
			detectedEndpoint := m.p.Endpoint
			done := m.fetchDone
			fetchCtx := m.nextFetchCtx()
			setDebugf("autoclaw detection skipped: built-in catalog endpoint=%q", probeEndpoint)
			go func() {
				msg := modelFetchDoneMsg{
					endpoint:            probeEndpoint,
					apiKey:              probeAPIKey,
					detectedType:        providerType,
					detectedEndpoint:    detectedEndpoint,
					discoveredModelsRaw: strings.Join(oauthproxy.AutoClawModelIDs(), ","),
					modelInfos:          oauthproxy.AutoClawModelCatalog(),
					catalogDiscovered:   true,
				}
				select {
				case done <- msg:
				case <-fetchCtx.Done():
				}
			}()
			return
		}
		m.live().detecting = true
		m.live().detectProgress = 5
		m.live().detectFrame = 0
		setDebugf("start detection endpoint=%q api_key_len=%d oauth=%t manual_models=%d", m.live().probeEndpoint, len(m.live().probeAPIKey), m.usesOAuth(), countCSV(manualModels))
		if m.canSelectCustomProtocol() {
			fetchModelsWithManualAsync(m.nextFetchCtx(), m.fetchDone, m.live().probeEndpoint, m.live().probeAPIKey, manualModels, preferManual, m.p.Type)
		} else {
			fetchModelsWithManualAsync(m.nextFetchCtx(), m.fetchDone, m.live().probeEndpoint, m.live().probeAPIKey, manualModels, preferManual)
		}
	case rowTestModels:
		if !m.connectionReady() || m.usesModelsDev() && !m.live().keyVerified {
			return
		}
		if len(m.live().modelPool) == 0 {
			setDebugf("model availability test skipped: empty pool")
			return
		}
		if m.modelTestCancel != nil {
			m.modelTestCancel()
			m.modelTestCancel = nil
		}
		m.modelTestID++
		testID := m.modelTestID
		ctx, cancel := context.WithCancel(m.lifecycleCtx())
		m.modelTesting = true
		m.modelTestCancel = cancel
		m.modelTestFrame = 0
		m.modelTestCanceled = false
		setDebugf("model availability test started model_count=%d", len(m.live().modelPool))
		testModelsAsync(m.availDone, ctx, testID, m.live().modelPool, m.live().probeEndpoint, m.live().probeAPIKey, probeWireType(*m.p), m.p.ModelProtocols, m.p.ModelEndpoints, m.availabilitySmokeTestModel(), m.usesModelsDev() && m.live().keyVerified)
	}
}

// handleFocusRow carries out the click semantics of a configuration row: the
// first click selects it (moves the cursor); a second click on the
// already-selected row performs its action. Endpoint and API Key focus their
// text inputs on first click so typing lands there.
func (m *AdvancedConfigModel) handleFocusRow(row configRowKind) {
	if row == rowCopyKey || row == rowCopyURL {
		// A single click on a value row focuses its input; a double-click
		// (second click on the same row within the window) copies the value.
		now := time.Now()
		double := row == m.lastCopyClickRow && now.Sub(m.lastCopyClickAt) < 500*time.Millisecond
		m.lastCopyClickRow = row
		m.lastCopyClickAt = now
		if !double {
			focus := rowAPIKey
			if row == rowCopyURL {
				focus = rowEndpoint
			}
			m.cursor = m.mainRowIndex(focus)
			m.urlFocused = false
			m.keyFocused = false
			m.modelsFocused = false
			m.markDirty()
			return
		}
		if row == rowCopyKey {
			m.keyCopied = true
			m.lastKeyCopyAt = now
			if err := clipboard.WriteAll(m.keyText.Get()); err != nil {
				setDebugf("copy key to clipboard failed: %v", err)
			}
			setDebugf("key copied to clipboard")
			m.markDirty()
			return
		}
		m.urlCopied = true
		m.lastUrlCopyAt = now
		if err := clipboard.WriteAll(m.urlText.Get()); err != nil {
			setDebugf("copy url to clipboard failed: %v", err)
		}
		setDebugf("url copied to clipboard")
		m.markDirty()
		return
	}
	idx := m.mainRowIndex(row)
	if idx < 0 {
		return
	}
	wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
	alreadySelected := m.cursor == idx && !m.textInputHasKeyboard()
	m.cursor = idx
	m.keepCursorVisible()
	m.urlFocused = false
	m.keyFocused = false
	m.modelsFocused = false
	switch row {
	case rowEndpoint:
		m.urlFocused = true
		m.refreshConnectionDirty()
	case rowAPIKey:
		m.keyFocused = true
		m.refreshConnectionDirty()
	case rowModelPool:
		if m.source == sourceCustom {
			m.modelsFocused = true
			m.refreshConnectionDirty()
		}
	case rowTest, rowTestModels, rowProvider, rowSource, rowDefaultModel:
		if alreadySelected {
			m.activateRow(row)
		}
	case rowSave:
		if alreadySelected && m.requestSave() {
			m.quit()
		}
	case rowCancel:
		if alreadySelected {
			setDebugf("click cancel requested")
			m.quit()
		}
	}
	if wasKeyRow && row != rowAPIKey {
		m.startModelsDevVerification()
	}
	m.markDirty()
}

// handleKey routes one key press. The models.dev picker and the two modal
// states (connection check, model test) own the keyboard while active; after
// that the endpoint/key inputs take printable keys, and the remaining keys
// move the page cursor.
func (m *AdvancedConfigModel) handleKey(ke tui.KeyEvent) {
	if m.width > 0 && m.height > 0 && (m.width < 32 || m.height < 8) && ke.Key != tui.KeyEscape && !(ke.Mod == tui.ModCtrl && ke.Rune == 'c') {
		return
	}
	if m.modelsDevPicker {
		m.handleModelsDevPickerKey(ke)
		m.markDirty()
		return
	}
	if m.defaultModelPicker {
		m.handleDefaultModelPickerKey(ke)
		m.markDirty()
		return
	}

	if ke.Mod == tui.ModCtrl && ke.Rune == 'c' {
		m.quit()
		return
	}

	// q 是 quit 的单字母别名，仅当没有文本输入框持有键盘时生效（q 也是
	// 合法的输入字符，见下方的 runeAlias 说明）。
	if ke.IsRune() && ke.Mod == 0 && ke.Rune == 'q' && !m.textInputHasKeyboard() {
		m.quit()
		return
	}

	// 模态：连接检查/模型测试进行中。esc 取消操作（或退出等待），其余按键
	// 等待结束。ctrl+c 已在上面处理。
	if m.live().detecting && !m.usesModelsDev() {
		if ke.Key == tui.KeyEscape {
			m.cancelFetch()
			m.live().detecting = false
			m.live().detectionError = fmt.Errorf("%s", locale.T("已取消连接检查", "connection check canceled"))
			m.focusDetectionAction()
			setDebugf("connection check canceled by user")
			m.markDirty()
		}
		return
	}
	if m.modelTesting {
		if ke.Key == tui.KeyEscape {
			if m.modelTestCancel != nil {
				m.modelTestCancel()
			}
			m.modelTesting = false
			m.modelTestCancel = nil
			m.modelTestCanceled = true
			setDebugf("model availability test canceled test_id=%d", m.modelTestID)
			m.markDirty()
		}
		return
	}

	// 文本输入框拥有键盘时，单字母导航别名让位给输入本身：q/h/j/k/l 是合法
	// 的输入字符。方向键没有这个歧义。
	inputHasKeyboard := m.textInputHasKeyboard()

	// vim 单字母别名 h/j/k/l：无输入焦点时映射到方向键。
	if m.handleNavAlias(ke, inputHasKeyboard) {
		return
	}

	switch ke.Key {
	case tui.KeyEscape:
		setDebugf("esc quit cursor=%d endpoint_set=%t api_key_len=%d", m.cursor, strings.TrimSpace(m.urlText.Get()) != "", len(m.keyText.Get()))
		m.quit()
		return

	case tui.KeyPageUp, tui.KeyPageDown:
		delta := max(scrollBodyBudget(m.height)/2, 1)
		if ke.Key == tui.KeyPageUp {
			delta = -delta
		}
		wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
		m.urlFocused, m.keyFocused, m.modelsFocused = false, false, false
		m.cursor = min(max(m.cursor+delta, 0), max(len(m.visibleRows())-1, 0))
		m.keepCursorVisible()
		if wasKeyRow {
			m.startModelsDevVerification()
		}
		m.markDirty()
		return
	case tui.KeyUp:
		if inputHasKeyboard && ke.IsRune() {
			return // navigation alias yielded to text input
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		m.urlFocused, m.keyFocused, m.modelsFocused = false, false, false
		wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
		if m.cursor > 0 {
			m.cursor--
		} else {
			m.cursor = len(rows) - 1
		}
		m.keepCursorVisible()
		if wasKeyRow {
			m.startModelsDevVerification()
		}
		m.markDirty()
		return

	case tui.KeyDown:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		m.urlFocused, m.keyFocused, m.modelsFocused = false, false, false
		wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
		if m.cursor < len(rows)-1 {
			m.cursor++
		} else {
			m.cursor = 0
		}
		m.keepCursorVisible()
		if wasKeyRow {
			m.startModelsDevVerification()
		}
		m.markDirty()
		return

	case tui.KeyLeft:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		switch m.currentRow() {
		case rowSource, rowDefaultModel, rowContext, rowProtocol, rowFast, rowActive:
			m.adjustReviewField(-1)
		}
		m.markDirty()
		return

	case tui.KeyRight:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		switch m.currentRow() {
		case rowSource, rowDefaultModel, rowContext, rowProtocol, rowFast, rowActive:
			m.adjustReviewField(1)
		}
		m.markDirty()
		return

	case tui.KeyEnter:
		if m.usesModelsDev() && m.currentRow() == rowAPIKey {
			m.keyFocused = false
			m.retryModelsDevVerification()
			m.focusDetectionAction()
			m.keepCursorVisible()
			m.markDirty()
			return
		}
		m.handleEnter()
		m.markDirty()
		return

	case tui.KeyTab:
		if inputHasKeyboard && ke.IsRune() {
			return
		}
		rows := m.visibleRows()
		if len(rows) == 0 {
			return
		}
		wasKeyRow := m.usesModelsDev() && m.currentRow() == rowAPIKey
		if ke.Mod == tui.ModShift {
			m.cursor--
			if m.cursor < 0 {
				m.cursor = len(rows) - 1
			}
		} else {
			m.cursor = (m.cursor + 1) % len(rows)
		}
		m.urlFocused, m.keyFocused, m.modelsFocused = false, false, false
		m.keepCursorVisible()
		if wasKeyRow {
			m.startModelsDevVerification()
		}
		m.markDirty()
		return
	}

	// space：非输入行上切换激活开关（输入框聚焦时作为空格字符输入）。
	if ke.Key == tui.KeyRune && ke.Rune == ' ' && ke.Mod == 0 && !inputHasKeyboard {
		if m.currentRow() == rowActive && m.connectionReady() {
			m.IsActiveChosen = !m.IsActiveChosen
		}
		m.markDirty()
		return
	}

	// 文本输入路由：光标在 Endpoint / API Key / Model Pool 行时，可打印字符进入对应文本。
	if ke.IsRune() && ke.Mod == 0 && ke.Rune != 0 {
		ch := string(ke.Rune)
		switch {
		case m.currentRow() == rowEndpoint && !m.usesOAuth():
			m.urlFocused = true
			m.keyFocused = false
			m.modelsFocused = false
			m.urlText.Set(m.urlText.Get() + ch)
			m.refreshConnectionDirty()
		case m.currentRow() == rowAPIKey && !m.usesOAuth():
			m.urlFocused = false
			m.keyFocused = true
			m.modelsFocused = false
			m.keyText.Set(m.keyText.Get() + ch)
			m.refreshConnectionDirty()
			if m.usesModelsDev() {
				m.invalidateModelsDevKeyIfChanged()
			}
		case m.currentRow() == rowModelPool && m.source == sourceCustom:
			m.urlFocused = false
			m.keyFocused = false
			m.modelsFocused = true
			if m.modelsText == nil {
				m.modelsText = tui.NewState("")
			}
			m.modelsText.Set(m.modelsText.Get() + ch)
			m.onCustomModelsEdited()
		default:
			// 光标在按钮或只读行上时，取消输入框的焦点。
			if !inputHasKeyboard {
				m.urlFocused = false
				m.keyFocused = false
				m.modelsFocused = false
			}
		}
		m.markDirty()
		return
	}

	if ke.Key == tui.KeyBackspace {
		switch {
		case m.currentRow() == rowEndpoint && !m.usesOAuth():
			t := m.urlText.Get()
			if t != "" {
				m.urlText.Set(removeLastRune(t))
			}
			m.refreshConnectionDirty()
		case m.currentRow() == rowAPIKey && !m.usesOAuth():
			before := m.keyText.Get()
			t := before
			if t != "" {
				m.keyText.Set(removeLastRune(t))
			}
			m.refreshConnectionDirty()
			if m.usesModelsDev() && m.keyText.Get() != before {
				m.invalidateModelsDevKeyIfChanged()
			}
		case m.currentRow() == rowModelPool && m.source == sourceCustom:
			if m.modelsText != nil {
				t := m.modelsText.Get()
				if t != "" {
					m.modelsText.Set(removeLastRune(t))
				}
				m.onCustomModelsEdited()
			}
		}
		m.markDirty()
		return
	}

	// 其余按键（非打印导航在上方处理过）：确保输入框焦点不残留。
	if !inputHasKeyboard {
		m.urlFocused = false
		m.keyFocused = false
		m.modelsFocused = false
		m.markDirty()
	}
}

func (m *AdvancedConfigModel) onCustomModelsEdited() {
	if m.modelsText == nil {
		return
	}
	raw := m.modelsText.Get()
	m.live().inputModels = raw
	normalized := normalizeModelCSV(raw)
	m.live().manualModelsEdited = normalized != ""
	m.live().modelPool = uniqueBaseModels(parseModelList(normalized))
	m.p.Model = normalized
	m.reconcileDefaultModelWithPool()
	m.refreshConnectionDirty()
}

// handleNavAlias maps the vim single-letter navigation aliases (h/j/k/l) onto
// the same key events the arrow keys produce. It runs before the text-input
// routing but only when no text input owns the keyboard, so the aliases stay
// typeable inside the endpoint/API key/filter inputs.
func (m *AdvancedConfigModel) handleNavAlias(ke tui.KeyEvent, inputHasKeyboard bool) bool {
	if inputHasKeyboard || !ke.IsRune() || ke.Mod != 0 {
		return false
	}
	switch ke.Rune {
	case 'k':
		m.handleKey(tui.KeyEvent{Key: tui.KeyUp})
	case 'j':
		m.handleKey(tui.KeyEvent{Key: tui.KeyDown})
	case 'h':
		m.handleKey(tui.KeyEvent{Key: tui.KeyLeft})
	case 'l':
		m.handleKey(tui.KeyEvent{Key: tui.KeyRight})
	default:
		return false
	}
	return true
}

// handleEnter performs the Enter action of the row under the cursor.
func (m *AdvancedConfigModel) handleEnter() {
	switch m.currentRow() {
	case rowSource:
		m.switchSource(m.otherSource())
	case rowEndpoint:
		m.cursor = m.mainRowIndex(rowAPIKey)
		m.urlFocused = false
		m.keyFocused = true
		setDebugf("enter endpoint -> api key endpoint=%q", m.urlText.Get())
	case rowAPIKey:
		// Custom advances to Auto Configure; models.dev verifies the key in
		// the background and advances to Context.
		if m.usesModelsDev() {
			m.retryModelsDevVerification()
		}
		m.focusDetectionAction()
		m.urlFocused = false
		m.keyFocused = false
		setDebugf("enter api key -> next api_key_len=%d", len(m.keyText.Get()))
	case rowProvider:
		m.activateRow(rowProvider)
	case rowModelPool:
		m.modelsFocused = false
		if strings.TrimSpace(m.urlText.Get()) == "" {
			m.cursor = m.mainRowIndex(rowEndpoint)
			m.urlFocused = true
		} else if strings.TrimSpace(m.keyText.Get()) == "" {
			m.cursor = m.mainRowIndex(rowAPIKey)
			m.keyFocused = true
		} else if m.modelsText != nil && countCSV(m.modelsText.Get()) > 0 {
			m.activateRow(rowTest)
		} else {
			m.focusDetectionAction()
		}
	case rowTest:
		m.activateRow(rowTest)
	case rowProtocol, rowFast:
		m.adjustReviewField(1)
	case rowDefaultModel:
		m.activateRow(rowDefaultModel)
	case rowTestModels:
		m.activateRow(rowTestModels)
	case rowContext:
		// Context & Compact is edited inline; nothing to open yet.
		setDebugf("context row selected")
	case rowActive:
		m.IsActiveChosen = !m.IsActiveChosen
		setDebugf("active choice toggled active_chosen=%t", m.IsActiveChosen)
	case rowSave:
		if m.requestSave() {
			m.quit()
		}
	case rowCancel:
		setDebugf("cancel requested")
		m.quit()
	}
	m.keepCursorVisible()
}

// KeyMap routes every key through handleKey. go-tui matches one binding per
// event, so a single AnyKey stop binding covers the whole page (the picker,
// the modals, and the inputs are disambiguated inside handleKey).
func (m *AdvancedConfigModel) KeyMap() tui.KeyMap {
	return tui.KeyMap{
		tui.OnStop(tui.AnyKey, func(ke tui.KeyEvent) {
			m.handleKey(ke)
			m.markDirty()
		}),
	}
}

// HandleMouse maps a click to the configuration row under the pointer using
// structured bodyLines() metadata and shared bodyLayoutGeometry() coordinates
// (no offline tui.Sprint re-render or ANSI text scraping).
func (m *AdvancedConfigModel) HandleMouse(me tui.MouseEvent) bool {
	if m.modelsDevPicker || m.defaultModelPicker || (m.live().detecting && !m.usesModelsDev()) || m.modelTesting {
		return false
	}
	if me.Button == tui.MouseWheelUp || me.Button == tui.MouseWheelDown {
		key := tui.KeyUp
		if me.Button == tui.MouseWheelDown {
			key = tui.KeyDown
		}
		m.handleKey(tui.KeyEvent{Key: key})
		return true
	}
	if me.Button != tui.MouseLeft || me.Action != tui.MousePress {
		return false
	}
	lines := m.bodyLines()
	topY, leftX, offset, visibleCount := m.bodyLayoutGeometry(len(lines))
	if me.X < leftX || me.X >= leftX+m.bodyWidth() {
		return false
	}
	visIdx := me.Y - topY
	if visIdx < 0 || visIdx >= visibleCount {
		return false
	}
	bodyIdx := offset + visIdx
	if bodyIdx < 0 || bodyIdx >= len(lines) {
		return false
	}
	l := lines[bodyIdx]
	if !l.hasKind {
		return false
	}
	row := l.kind
	if l.hasSecondary && me.X >= leftX+l.secondaryCol {
		row = l.secondaryKind
	}
	m.handleFocusRow(row)
	return true
}

// Watchers bridges the async goroutines onto the main loop: the four result
// channels, the two spinner/cleanup timers, and the one-shot auto-detect that
// used to live in Init(). go-tui calls Watchers() after the first render, so
// the channels are guaranteed to be watched before any fetch can complete.
func (m *AdvancedConfigModel) Watchers() []tui.Watcher {
	m.startAutoDetect()
	return []tui.Watcher{
		tui.Watch(m.fetchDone, m.handleFetchDone),
		tui.Watch(m.verifyDone, m.handleVerifyDone),
		tui.Watch(m.mdDone, m.handleModelsDevDone),
		tui.Watch(m.availDone, m.handleAvailabilityDone),
		tui.Watch(m.oauthDone, m.handleOAuthRuntimeDone),
		tui.OnTimer(120*time.Millisecond, m.handleFetchTick),
		tui.OnTimer(120*time.Millisecond, m.handleAvailabilityTick),
	}
}
