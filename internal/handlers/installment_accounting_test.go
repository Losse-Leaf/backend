package handlers

import "testing"

// DEC-01 口径 B：分期款**逐期**扣现金 → 净资产在整个分期周期内**恒定**
//
// 原先是两套模型混用：
//   - 流动可用余额把**全部分期支出排除**（分期买东西从不扣现金）
//   - 待还信贷总额却**随还款逐月减少**
//
// 凑一起 ⇒ 净资产随还款进度虚增，涨的幅度恰好等于已还金额。
func TestInstallmentNetWorthStaysConstant(t *testing.T) {
	const (
		total      = int64(1200000) // ¥12,000
		months     = 12
		monthlyPay = int64(100000) // ¥1,000 / 期
	)

	// 固定资产（分期买入的设备）与初始流动资金
	const fixedAssets = int64(1200000)
	const initialLiquid = int64(1000000) // ¥10,000

	for paidMonths := 0; paidMonths <= months; paidMonths++ {
		remainMonths := months - paidMonths
		paidAmount, debt := CalculateInstallmentAmounts(total, months, monthlyPay, paidMonths, remainMonths)

		// 口径 B：现金逐期扣
		liquid := initialLiquid - paidAmount
		// 固资不变，负债随还款减少
		liabilities := debt
		netWorth := liquid + fixedAssets - liabilities

		want := int64(1000000) // ¥10,000，整个周期恒定
		if netWorth != want {
			t.Errorf("已还 %d 期时净资产应为 %d，实际 %d（已还 %d，待还 %d）",
				paidMonths, want, netWorth, paidAmount, debt)
		}
	}
}

// DEC-02 口径 A：前 N-1 期整除，最后一期吃掉尾差 → 各期之和 == 总额
func TestInstallmentPeriodAmountAbsorbsRemainder(t *testing.T) {
	const (
		total     = int64(916900) // ¥9,169.00
		months    = 12
		monthly   = int64(76408) // 916900 / 12 = 76408 余 4
		remainder = total % int64(months)
	)
	if remainder != 4 {
		t.Fatalf("本用例假定余数为 4，实际 %d", remainder)
	}

	sum := int64(0)
	for period := 1; period <= months; period++ {
		amount := InstallmentPeriodAmount(total, months, monthly, period)
		sum += amount
		if period < months && amount != monthly {
			t.Errorf("第 %d 期应为整除值 %d，实际 %d", period, monthly, amount)
		}
	}
	if sum != total {
		t.Errorf("各期之和应等于总额 %d，实际 %d", total, sum)
	}

	// 最后一期必须多出尾差
	last := InstallmentPeriodAmount(total, months, monthly, months)
	if last != monthly+remainder {
		t.Errorf("最后一期应为 %d，实际 %d", monthly+remainder, last)
	}
	// 越界保护
	if InstallmentPeriodAmount(total, months, monthly, 0) != monthly {
		t.Error("期数 < 1 应按第 1 期处理")
	}
	if InstallmentPeriodAmount(total, months, monthly, months+5) != monthly+remainder {
		t.Error("期数超出应按最后一期处理")
	}
}
