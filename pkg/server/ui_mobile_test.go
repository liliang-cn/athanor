package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/liliang-cn/athanor/pkg/livedb"
)

// What a phone needs, asserted where a machine can assert it.
//
// go test cannot tell anybody a page is usable on a phone; a person with a
// phone does that, and these were written beside that reading rather than
// instead of it. What is here is the half that rots silently: a screen that
// gains a table next year, or a column, and does not gain the label the card
// treatment reads, degrades into an unlabelled stack of values and nothing
// says so. So these assert the four structural claims the treatment rests on
// — the breakpoint exists, every card cell carries its label, every control
// declares a font iOS will not zoom, and nothing declares a pixel width wider
// than the narrowest phone — against the real bytes the handlers write.

// theBreakpoint is the one media query, and it is named here rather than
// matched loosely so that moving it is a decision somebody makes on purpose.
const theBreakpoint = "@media (max-width:760px)"

// phoneWidth is the artboard: 390 CSS pixels, an iPhone 14/15/16 upright and
// the narrowest screen these pages are drawn for.
const phoneWidth = 390

// mobileScreens is every screen a signed-in browser can reach, fetched as one
// so that a claim is made about all of them rather than about a favourite.
func mobileScreens(t *testing.T, h *harness) map[string]string {
	t.Helper()
	decision := loadDecision(t, h, "op-secret", uploadAndCreate(t, h), "march")
	paths := []string{
		"/",
		"/app/shelf",
		"/app/shelf?grade=verified",
		"/app/decisions",
		"/app/decisions/" + decision,
		"/app/import",
		"/app/import/livedb",
		"/app/import/runs",
		"/app/import/follows",
	}
	pages := make(map[string]string, len(paths))
	for _, path := range paths {
		code, body := h.browse(t, path, "op-secret")
		if code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", path, code, body)
		}
		pages[path] = body
	}
	return pages
}

func TestEveryScreenCarriesThePhoneBreakpointAndTheViewportToApplyIt(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	for path, page := range mobileScreens(t, h) {
		if !strings.Contains(page, theBreakpoint) {
			t.Errorf("%s has no %s block: it is a desktop page that merely gets narrower", path, theBreakpoint)
		}
		// Without this a mobile browser lays the page out at 980px and zooms
		// out, and every rule below the breakpoint never fires. The front
		// page went a long time without one.
		if !strings.Contains(page, `name="viewport"`) {
			t.Errorf("%s declares no viewport, so a phone renders it at 980px and scales it down", path)
		}
	}
}

func TestTheBottomBarHoldsEveryDestinationAndMarksTheCurrentOne(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	pages := mobileScreens(t, h)
	for path, page := range pages {
		if path == "/" {
			continue // the front page is not drawn in the shell.
		}
		bar := between(page, `<div class="rail-links">`, `</div>`)
		if bar == "" {
			t.Fatalf("%s has no rail-links element, so there is nothing to fix to the bottom of a phone", path)
		}
		for _, item := range uiNav {
			if !strings.Contains(bar, `href="`+item.Href+`"`) {
				t.Errorf("%s: the bar is missing %s — a destination a thumb cannot reach is not in the product", path, item.Href)
			}
		}
		if n := strings.Count(bar, `aria-current="page"`); n != 1 {
			t.Errorf("%s marks %d entries as current, want exactly one", path, n)
		}
		// Sign out is deliberately not in the bar: it is the one control that
		// ends the session and it should not sit a mis-tap from the one that
		// moves between screens.
		if strings.Contains(bar, "/signout") {
			t.Errorf("%s put sign-out in the thumb bar", path)
		}
	}
}

var (
	tableOpen = regexp.MustCompile(`(?s)<table([^>]*)>(.*?)</table>`)
	cellOpen  = regexp.MustCompile(`<td([^>]*)>`)
	rowOpen   = regexp.MustCompile(`(?s)<tr([^>]*)>(.*?)(?:</tr>|(?:\s*<tr)|$)`)
)

func TestEveryCardCellCarriesTheLabelTheCardTreatmentNeeds(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	cards := map[string]int{}
	for path, page := range mobileScreens(t, h) {
		for _, table := range tableOpen.FindAllStringSubmatch(page, -1) {
			if !strings.Contains(table[1], `class="cards"`) {
				continue
			}
			cards[path]++
			// The header row is hidden below the breakpoint — the labels it
			// carried are on the cells instead — so it has to be findable.
			if !strings.Contains(table[2], `<tr class="hd">`) {
				t.Errorf("%s: a cards table has no hd row, so its column headings stay on a phone", path)
			}
			for _, cell := range cellOpen.FindAllStringSubmatch(table[2], -1) {
				if !strings.Contains(cell[1], "data-label=") {
					t.Errorf("%s: <td%s> carries no data-label, so as a card it is a value with nothing saying what it is",
						path, cell[1])
				}
			}
		}
	}
	// The three screens whose data is rows rather than pairs. A section with
	// nothing in it renders a sentence instead of a table, so this names the
	// screens that always have one rather than counting tables in the round.
	for _, path := range []string{"/", "/app/shelf", "/app/decisions"} {
		if cards[path] == 0 {
			t.Errorf("%s emits no card table at all, so its rows are still a table on a phone", path)
		}
	}
}

func TestTheOnlyTablesLeftAsTablesArePairsOrDeliberatelyWide(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	for path, page := range mobileScreens(t, h) {
		for _, table := range tableOpen.FindAllStringSubmatch(page, -1) {
			switch {
			case strings.Contains(table[1], `class="cards"`), strings.Contains(table[1], `class="wide"`):
				continue
			}
			// What is left must be a label/value table — one <th> per row —
			// which already reads as a card without any help. A row-shaped
			// table that slipped through would be a five-column crush.
			for _, row := range rowOpen.FindAllStringSubmatch(table[2], -1) {
				if n := strings.Count(row[2], "<th"); n != 1 {
					t.Errorf("%s: an unclassed table has a row with %d headers, so it is row-shaped and needs the card treatment:\n%s",
						path, n, strings.TrimSpace(row[2]))
				}
			}
		}
	}
}

func TestEveryScreenTellsIOSNotToZoomTheFormFields(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	for path, page := range mobileScreens(t, h) {
		block := mobileBlock(page)
		if block == "" {
			t.Fatalf("%s: no %s block to read", path, theBreakpoint)
		}
		found := false
		for _, rule := range strings.Split(block, "}") {
			selector, body, ok := strings.Cut(rule, "{")
			if !ok || !strings.Contains(selector, "input") {
				continue
			}
			if size := fontSize(body); size >= 16 {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no rule below the breakpoint gives an input a font of 16px or more, "+
				"which is the whole of why iOS Safari zooms a page when a field takes focus", path)
		}
	}
}

var pxWidth = regexp.MustCompile(`(?:^|[;{\s])(?:min-|max-)?width:\s*(\d+(?:\.\d+)?)px`)

func TestNoScreenDeclaresAFixedWidthWiderThanThePhone(t *testing.T) {
	h := newHarness(t, fakeRunner{result: cannedResult()})
	for path, page := range mobileScreens(t, h) {
		for _, m := range pxWidth.FindAllStringSubmatch(page, -1) {
			px, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				continue
			}
			if px > phoneWidth {
				t.Errorf("%s declares %spx of fixed width, which is wider than a %dpx phone and is how a body "+
					"comes to scroll sideways", path, m[1], phoneWidth)
			}
		}
	}
}

// The live-database plan is the one table that stays a table: six columns of
// which four are read by comparing them against each other, and a card breaks
// every one of those comparisons. It keeps its sideways scroll, so what this
// asserts is that the scroll is real — a container that clips, a table with a
// width to scroll to — and that the container says so without a word.
func TestThePlanTableKeepsAScrollThatSaysItIsThere(t *testing.T) {
	view := livedbWizardView{
		Drivers: []string{"postgres", "mysql"}, Actions: livedbUIActions, Driver: "postgres",
		Plan: &livedbPlanView{
			ID: "plan_0caa2dfa", State: string(livedb.Draft), Draft: true,
			Redacted: "postgres://app:***@db.example.com:5432/payments", Driver: "postgres",
			Tables: []string{"customers"}, Hash: "sha256:3f1c9a77",
			Counts: []livedbCountRow{{Label: "columns", N: 1}, {Label: "personal", N: 1}},
			Columns: []livedbColumnRow{{
				Table: "customers", Column: "customer_email", Type: "varchar(255)",
				Kind: "email", Sensitivity: "confidential", Action: "redact",
				Sample: "a***@example.com", Enters: "nothing", Actions: livedbUIActions,
			}},
		},
	}
	body, err := uiRender(livedbWizardTmpl, view)
	if err != nil {
		t.Fatalf("the wizard: %v", err)
	}
	page := string(body)
	if !strings.Contains(page, `<div class="scroll"><table class="wide">`) {
		t.Fatalf("the plan's columns are not a wide table inside a scroll box:\n%s", page)
	}
	// The counts beside it are not wide, and go to cards like everything else.
	if !strings.Contains(page, `<table class="cards">`) || !strings.Contains(page, `data-label="columns"`) {
		t.Errorf("the plan's counts did not get the card treatment:\n%s", page)
	}

	block := mobileBlock(uiCSS)
	for _, want := range []string{".wide{min-width:", "overscroll-behavior-x:contain", "background-attachment:local"} {
		if !strings.Contains(block, want) {
			t.Errorf("the mobile block has no %q, so the wide table either does not scroll or does not show that it can", want)
		}
	}
}

// between returns what lies between the first open and the next close.
func between(page, open, close string) string {
	_, rest, ok := strings.Cut(page, open)
	if !ok {
		return ""
	}
	inner, _, ok := strings.Cut(rest, close)
	if !ok {
		return ""
	}
	return inner
}

// mobileBlock is the media query's own body, brace-balanced, because the
// rules inside it carry braces of their own.
func mobileBlock(css string) string {
	_, rest, ok := strings.Cut(css, theBreakpoint+"{")
	if !ok {
		return ""
	}
	depth := 1
	for i, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i]
			}
		}
	}
	return ""
}

// fontSize reads the largest font-size a declaration block sets, in pixels.
// A rule that sets it in anything but px is not answering the question this
// test asks, which is a question about one number iOS Safari compares against.
func fontSize(body string) float64 {
	var largest float64
	for _, decl := range strings.Split(body, ";") {
		prop, value, ok := strings.Cut(decl, ":")
		if !ok || strings.TrimSpace(prop) != "font-size" {
			continue
		}
		value = strings.TrimSpace(value)
		if !strings.HasSuffix(value, "px") {
			continue
		}
		if px, err := strconv.ParseFloat(strings.TrimSuffix(value, "px"), 64); err == nil && px > largest {
			largest = px
		}
	}
	return largest
}
