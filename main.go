package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"github.com/joho/godotenv"
	"github.com/samber/lo"
	lop "github.com/samber/lo/parallel"
)

const domain = "https://lms.ssu.ac.kr"
const loginPage = "https://smartid.ssu.ac.kr/Symtra_sso/smln.asp?apiReturnUrl=https://lms.ssu.ac.kr/xn-sso/gw-cb.php"

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	id := os.Getenv("ID")
	pw := os.Getenv("PW")

	allocCtx, cancel := NewAllocator()
	defer cancel()

	tab, _ := NewTab(allocCtx)

	tab.GotoURL(loginPage)
	tab.Fill("#userid", id+"\t"+pw+"\n", &Opts{WaitResponse: true})
	tab.GotoURL(domain + "/mypage")
	iframe := tab.GetNode("iframe")

	time.Sleep(600 * time.Millisecond) // 최적화 완료
	var todoSubjects []*cdp.Node

	_, dur, _ := lo.WaitFor(func(_ int) bool {
		todoSubjects = lop.Map(
			tab.GetNodes(".xn-student-course-container", &Opts{Base: iframe, Timeout: 100 * time.Millisecond}),
			func(node *cdp.Node, i int) *cdp.Node {
				if tab.Text(" a", &Opts{Base: node, DisableLog: true}) != "0" {
					return node
				}
				return nil
			},
		)

		todoSubjects = lo.Compact(todoSubjects)

		return len(todoSubjects) > 0
	}, 60*time.Second, 1*time.Second)
	fmt.Println("싸강있는 강의 파악. 대기시간:", dur)

	var srcs []string

	subjectTabs := lop.Map(todoSubjects, func(subject *cdp.Node, index int) lo.Tuple3[*Tab, context.CancelFunc, []string] {
		time.Sleep(time.Duration(index) * time.Second) // TODO: time.Sleep 없애기
		tab.Click(" button", &Opts{Base: subject})     // 같은 탭의 버튼 여러개 클릭은 병렬화 불가
		titles := tab.Texts(".xnsti-left:has(.video)", &Opts{Base: subject, WaitVisible: true})

		titles = lo.Filter(titles, func(title string, _ int) bool {
			return !strings.Contains(title, "English") && !strings.Contains(title, "中文")
		})

		if len(titles) == 0 {
			return lo.Tuple3[*Tab, context.CancelFunc, []string]{}
		}

		subjectTab, cancel := tab.OpenInNewTab(".xntc-count", &Opts{Base: subject, LogTag: fmt.Sprint(subject.NodeID, " 과목탭")})

		return lo.T3(subjectTab, cancel, titles)
	})

	subjectTabs = lo.Filter(subjectTabs, func(tuple lo.Tuple3[*Tab, context.CancelFunc, []string], _ int) bool {
		// Tab이 nil이 아닌 경우만 유효한 것으로 간주
		return tuple.A != nil
	})

	lo.Map(subjectTabs, func(tuple lo.Tuple3[*Tab, context.CancelFunc, []string], index int) any {
		subjectTab, cancel, titles := tuple.A, tuple.B, tuple.C
		defer cancel()
		fmt.Println(strings.Join(titles, ","))

		time.Sleep(5 * time.Second) // TODO: time.Sleep 없애기 여기 왜 에러 -> 아마도 frame관련인거 같은데...
		iframe := subjectTab.GetNode("#tool_content")
		courseEntryNodeIds := lo.Filter(
			subjectTab.GetNodeIDs(".xnmb-module_item-wrapper:has(.readystream, .mp4, .everlec, .movie) a", &Opts{Base: iframe}),
			func(nodeId cdp.NodeID, _ int) bool {
				return lo.Contains(titles, subjectTab.Text(nodeId))
			},
		)

		lo.Map(courseEntryNodeIds, func(courseEntryNodeId cdp.NodeID, _ int) any {

			courseTab, cancel := subjectTab.OpenInNewTab(courseEntryNodeId, &Opts{LogTag: fmt.Sprint(courseEntryNodeId, " 강의탭")})
			defer cancel()

			time.Sleep(4 * time.Second) // TODO: time.Sleep 없애기

			src := courseTab.GetAttribute("iframe", "src", &Opts{Base: courseTab.GetNode("#tool_content")})
			srcs = append(srcs, src)

			return nil
		})
		return nil
	})

	lo.Map(srcs, func(src string, index int) any {
		videoTab, _ := NewTab(tab.Ctx)
		videoTab.GotoURL(src)

		videoTab.Click("[title='재생']")

		lo.Times(2, func(_ int) any {
			time.Sleep(2 * time.Second) // TODO: time.Sleep 없애기
			if okBtnId := videoTab.GetNodeID(".confirm-ok-btn", &Opts{WaitVisible: true, Timeout: 20 * time.Second}); okBtnId != 0 {
				videoTab.Click(okBtnId)
			}
			return nil
		})

		mem := -1

		for {
			time.Sleep(10 * time.Second)
			videoTab.Click(".vc-pctrl-on-playing", &Opts{Timeout: 1 * time.Second})
			videoTab.Click(".vc-pctrl-on-pause", &Opts{Timeout: 1 * time.Second})

			videoTab.Click(".vc-pctrl-playback-rate-toggle-btn", &Opts{Timeout: 1 * time.Second})
			videoTab.Click(".vc-pctrl-playback-rate-setbox>:nth-child(5)", &Opts{Timeout: 1 * time.Second})

			currentTime := videoTab.Text(".vc-pctrl-curr-time", &Opts{WaitVisible: true, Timeout: 1 * time.Second})
			totalDuration := videoTab.Text(".vc-pctrl-total-duration", &Opts{WaitVisible: true, Timeout: 1 * time.Second})

			if currentTime == "" || totalDuration == "" {
				continue
			}
			if convertTime(currentTime) >= convertTime(totalDuration) {
				break
			}

			if mem == convertTime(currentTime) {
				panic("예상치 못한 강의 멈춤 -> 프로그램 다시 시작")
			}
			mem = convertTime(currentTime)

			fmt.Println(index+1, "번째 강의", "재생중", currentTime, totalDuration)
		}

		return nil
	})

	fmt.Println("프로그램 종료")
}

var DefaultOpts = append(chromedp.DefaultExecAllocatorOptions[:],
	chromedp.Flag("mute-audio", true), // 오디오 끄기
	chromedp.Flag("start-maximized", true),
	chromedp.Flag("headless", IsProduction()),                       // 직접 브라우저 보기
	chromedp.Flag("disable-blink-features", "AutomationControlled"), // 자동화 흔적 없애기 -> 구글 검색 뚫기
	chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/85.0.4183.102 Safari/537.36"), // 자동화 흔적 없애기
	chromedp.NoSandbox,
	chromedp.Flag("disable-web-security", true),
	chromedp.Flag("disable-site-isolation-trials", true),
	chromedp.Flag("disable-beforeunload", true),
	chromedp.Flag("disable-popup-blocking", true),
	chromedp.Flag("disable-notifications", true),
	chromedp.Flag("disable-features", "IsolateOrigins,site-per-process"),
)

func NewAllocator() (context.Context, context.CancelFunc) {
	return chromedp.NewExecAllocator(context.Background(), DefaultOpts...)
}

func NewBrowser(allocatorCtx context.Context) (context.Context, context.CancelFunc) {
	return chromedp.NewContext(allocatorCtx)

}

func convertTime(timestring string) int {
	// 1:23 -> 83
	// 1:23:45 -> 5025
	parts := strings.Split(timestring, ":")
	slices.Reverse(parts)
	totalSeconds := 0

	for i, v := range parts {
		totalSeconds += powInt(60, i) * parseInt(v)
	}

	return totalSeconds
}

func powInt(x, y int) int {
	result := 1
	for i := 0; i < y; i++ {
		result *= x
	}
	return result
}

func parseInt(s string) int {
	result := 0
	for _, r := range s {
		result *= 10
		result += int(r - '0')
	}
	return result
}
