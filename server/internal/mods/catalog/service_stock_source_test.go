package catalog

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	storefrontv1 "github.com/NovaWorks/zcard-next/server/api/storefront/v1"
	"github.com/NovaWorks/zcard-next/server/internal/data/ent/card"
)

func TestStoreDetailKeepsLocalStockWithUpstreamAssociation(t *testing.T) {
	for _, scope := range []string{"product", "sku"} {
		for _, quantity := range []int{0, 3} {
			t.Run(fmt.Sprintf("%s/%d", scope, quantity), func(t *testing.T) {
				d, _ := newStatsEnv(t)
				ctx := context.Background()
				p := d.Client.Product.Create().SetName("本地发货商品").SetSlug("local-delivery").
					SetStatus(1).SetStockType("card").SetPrice(100).
					SetUpstreamSourceID(9).SetUpstreamProductCode("original-product").SaveX(ctx)
				skuID := uint64(0)
				if scope == "product" {
					p = d.Client.Product.UpdateOneID(p.ID).SetFulfillmentMode("local").SaveX(ctx)
				} else {
					sku := d.Client.ProductSku.Create().SetProductID(p.ID).SetName("默认规格").
						SetSpecValues(map[string]string{}).SetFulfillmentMode("local").SaveX(ctx)
					skuID = sku.ID
				}
				for i := 0; i < quantity; i++ {
					d.Client.Card.Create().SetProductID(p.ID).SetSkuID(skuID).
						SetStatus(card.StatusAvailable).SetContent([]byte("测试卡密")).
						SetContentHash(fmt.Sprintf("local-%d", i)).SaveX(ctx)
				}
				store := NewStoreCatalogService(NewCatalogUsecase(NewProductRepoImpl(d, nil)), nil, nil)
				calls := 0
				store.SetStockLookup(displayLookupFunc(func(context.Context, uint64, string) (int32, error) {
					calls++
					return -2, fmt.Errorf("上游认证失败")
				}))
				detail, err := store.GetProduct(ctx, &storefrontv1.GetProductRequest{Id: p.ID})
				if err != nil {
					t.Fatal(err)
				}
				if detail.Stock != int64(quantity) || detail.StockStatus != "current" || detail.StockReference != int64(quantity) {
					t.Fatalf("本地库存被上游覆盖：库存=%d，状态=%q，参考库存=%d", detail.Stock, detail.StockStatus, detail.StockReference)
				}
				if calls != 0 {
					t.Fatalf("本地发货不应查询上游，实际查询 %d 次", calls)
				}
			})
		}
	}
}

func TestStoreDetailKeepsFreshUpstreamSnapshot(t *testing.T) {
	d, _ := newStatsEnv(t)
	ctx := context.Background()
	p := d.Client.Product.Create().SetName("上游发货商品").SetSlug("upstream-delivery").
		SetStatus(1).SetStockType("card").SetPrice(100).
		SetUpstreamSourceID(9).SetUpstreamProductCode("original-product").SaveX(ctx)
	d.Client.SupplyMapping.Create().SetConnectionID(9).SetUpstreamProduct("original-product").
		SetLocalProductID(p.ID).SetUpStock(7).SetStockCheckedAt(time.Now().UTC()).SaveX(ctx)
	store := NewStoreCatalogService(NewCatalogUsecase(NewProductRepoImpl(d, nil)), nil, nil)
	calls := 0
	store.SetStockLookup(displayLookupFunc(func(context.Context, uint64, string) (int32, error) {
		calls++
		return 99, nil
	}))
	detail, err := store.GetProduct(ctx, &storefrontv1.GetProductRequest{Id: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Stock != 7 || detail.StockStatus != "current" || detail.StockReference != 7 {
		t.Fatalf("有效库存快照被重复查询覆盖：库存=%d，状态=%q，参考库存=%d", detail.Stock, detail.StockStatus, detail.StockReference)
	}
	if calls != 0 {
		t.Fatalf("有效库存快照不应重复查询上游，实际查询 %d 次", calls)
	}
}

func TestStoreDetailRefreshesStaleUpstreamSnapshotOnce(t *testing.T) {
	d, _ := newStatsEnv(t)
	ctx := context.Background()
	p := d.Client.Product.Create().SetName("上游库存刷新商品").SetSlug("stale-upstream").
		SetStatus(1).SetStockType("card").SetPrice(100).
		SetUpstreamSourceID(9).SetUpstreamProductCode("original-product").SaveX(ctx)
	d.Client.SupplyMapping.Create().SetConnectionID(9).SetUpstreamProduct("original-product").
		SetLocalProductID(p.ID).SetUpStock(7).SetStockCheckedAt(time.Now().Add(-time.Hour)).SaveX(ctx)
	store := NewStoreCatalogService(NewCatalogUsecase(NewProductRepoImpl(d, nil)), nil, nil)
	var calls atomic.Int32
	store.SetStockLookup(displayLookupFunc(func(context.Context, uint64, string) (int32, error) {
		calls.Add(1)
		return 12, nil
	}))
	detail, err := store.GetProduct(ctx, &storefrontv1.GetProductRequest{Id: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Stock != 12 || detail.StockStatus != "current" || detail.StockReference != 12 {
		t.Fatalf("上游过期库存未正确刷新：库存=%d，状态=%q，参考库存=%d", detail.Stock, detail.StockStatus, detail.StockReference)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("过期库存应只查询上游一次，实际查询 %d 次", n)
	}
}
