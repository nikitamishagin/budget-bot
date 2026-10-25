package handler

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("parseExpenseDate", func() {
	var loc *time.Location

	BeforeEach(func() {
		var err error
		loc, err = time.LoadLocation("Europe/Belgrade")
		Expect(err).NotTo(HaveOccurred())
	})

	utc := func(s string) int64 {
		t, err := time.Parse(time.RFC3339, s)
		Expect(err).NotTo(HaveOccurred())
		return t.Unix()
	}

	DescribeTable("valid input",
		func(in, wantUTC string) {
			got, ok := parseExpenseDate(in, loc)
			Expect(ok).To(BeTrue())
			Expect(got).To(Equal(utc(wantUTC)))
		},
		Entry("summer (CEST)", "2026.08.26 10:10", "2026-08-26T08:10:00Z"),
		Entry("winter (CET)", "2026.01.15 10:10", "2026-01-15T09:10:00Z"),
		Entry("dotted with seconds truncated", "2026.08.26 10:10:45", "2026-08-26T08:10:00Z"),
		Entry("dotted date only", "2026.08.26", "2026-08-25T22:00:00Z"),
		Entry("dashed with space", "2026-08-26 10:10", "2026-08-26T08:10:00Z"),
		Entry("dashed with T", "2026-08-26T10:10", "2026-08-26T08:10:00Z"),
		Entry("dashed date only", "2026-08-26", "2026-08-25T22:00:00Z"),
		Entry("day-first with time", "26.08.2026 10:10", "2026-08-26T08:10:00Z"),
		Entry("day-first date only", "26.08.2026", "2026-08-25T22:00:00Z"),
		Entry("surrounding whitespace", "  2026.08.26 10:10  ", "2026-08-26T08:10:00Z"),
		Entry("RFC3339 ignores loc", "2026-08-26T10:10:00+05:00", "2026-08-26T05:10:00Z"),
		Entry("RFC3339 seconds truncated", "2026-08-26T10:10:59Z", "2026-08-26T10:10:00Z"),
	)

	DescribeTable("invalid input",
		func(in string) {
			got, ok := parseExpenseDate(in, loc)
			Expect(ok).To(BeFalse())
			Expect(got).To(BeZero())
		},
		Entry("empty", ""),
		Entry("whitespace only", " \t\n "),
		Entry("garbage", "not a date"),
	)

	It("seconds give the same result as without", func() {
		a, _ := parseExpenseDate("2026.08.26 10:10:45", loc)
		b, _ := parseExpenseDate("2026.08.26 10:10", loc)
		Expect(a).To(Equal(b))
	})

	It("uses time.Local when loc is nil", func() {
		got, ok := parseExpenseDate("2026.08.26 10:10", nil)
		Expect(ok).To(BeTrue())
		want := time.Date(2026, 8, 26, 10, 10, 0, 0, time.Local).Unix()
		Expect(got).To(Equal(want))
	})
})
