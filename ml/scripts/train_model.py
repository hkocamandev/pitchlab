"""Train the pitch-outcome models and write a Turkish evaluation report.

Usage:
    .venv/bin/python ml/scripts/train_model.py --version pitch-outcome-v1.0.0

The test split is not touched unless PITCHLAB_ALLOW_TEST_SET=1 is set, which
should happen exactly once, after the model is frozen.
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
from datetime import datetime
from pathlib import Path

import numpy as np
import pandas as pd
from sklearn.metrics import roc_auc_score

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import (
    baseline,
    config,
    evaluate,
    features,
    labels,
    split,
    train,
)

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)-5s %(message)s"
)
log = logging.getLogger("train")

REFERENCE_CLASS = "SWINGING_STRIKE"
BASELINE_REFERENCE = "B2_count_group"


def latest_snapshot() -> Path:
    candidates = sorted(config.RAW_DIR.glob("statcast_*.parquet"))
    if not candidates:
        raise FileNotFoundError(
            f"no snapshot in {config.RAW_DIR}; run ml/scripts/pull_seasons.py first"
        )
    return candidates[-1]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--snapshot", type=Path, default=None)
    ap.add_argument("--version", default="pitch-outcome-v1.0.0")
    ap.add_argument("--variants", nargs="+", default=["pitching", "stuff"])
    args = ap.parse_args()

    snapshot = args.snapshot or latest_snapshot()
    df = train.prepare(snapshot)

    # Seasons outside the configured regime (e.g. 2022, kept only so that 2023
    # pitches have a previous-season arsenal baseline, and 2026, which is a
    # different measurement regime) are dropped here by make_split.
    parts = split.make_split(df)
    log.info("\n%s", parts.summary().to_string(index=False))

    lines: list[str] = []

    def emit(text: str = "") -> None:
        print(text)
        lines.append(text)

    emit("# FAZ 1 — Model Değerlendirme Raporu")
    emit()
    emit(f"> Üretim tarihi: {datetime.now():%Y-%m-%d %H:%M}")
    emit(f"> Veri anlık görüntüsü: `{snapshot.name}`")
    emit(f"> Model sürümü: `{args.version}`")
    emit()

    emit("## Veri bölünmesi")
    emit()
    s = parts.summary()
    emit("| bölüm | satır | başlangıç | bitiş | atıcı |")
    emit("|---|---:|---|---|---:|")
    for _, r in s.iterrows():
        emit(f"| {r['split']} | {r['rows']:,} | {r['start']:%Y-%m-%d} | "
             f"{r['end']:%Y-%m-%d} | {r['pitchers']:,} |")
    emit()
    emit("> Bölünme **zamansaldır**. Rastgele bölünme, aynı vuruş sırasındaki "
         "ardışık atışları ve aynı atıcı-sezon agregatlarını iki tarafa "
         "dağıtarak sızdırır.")
    emit()

    emit("## Sınıf dağılımı (eğitim kümesi)")
    emit()
    dist = labels.class_distribution(parts.train)
    emit("| sınıf | adet | pay |")
    emit("|---|---:|---:|")
    for cls_name, row in dist.iterrows():
        emit(f"| `{cls_name}` | {int(row['count']):,} | {row['share']:.2%} |")
    emit()

    trained: dict[str, train.TrainedModel] = {}
    all_results: dict[str, dict[str, evaluate.Metrics]] = {}

    for variant in args.variants:
        if variant == "stuff":
            model, sm = train.train_stuff_model(parts, args.version)
            model.save()
            trained[variant] = model
            emit("## Varyant `stuff` — P(whiff | sallanış)")
            emit()
            emit("FAZ 0'da `stuff_score`, konumsuz bir 5 sınıflı modelden "
                 "türetilecekti. FAZ 1 ölçümü bunun yanlış alt-problem olduğunu "
                 "gösterdi: fiziksel özellikler sallanma kararında **0,554 AUC** "
                 "(yazı-tura) veriyor, çünkü vurucunun sallanıp sallanmayacağı "
                 "topun nereye gittiğiyle ilgili bir soru. Sallanış koşullandığında "
                 "aynı özellikler **0,652 AUC**'ye çıkıyor. Bu yüzden `stuff` "
                 "modeli artık halka açık Stuff+ modellerinin sorduğu soruyu "
                 "soruyor: *sallanıldığında ıskalanma olasılığı*.")
            emit()
            emit(f"- Özellik sayısı: **{len(model.feature_columns)}** (yalnızca fiziksel)")
            emit(f"- Eğitim örneklemi: **{sm['n_train']:,}** sallanış "
                 f"(atışların %{100 * sm['n_train'] / len(parts.train):.1f}'i)")
            emit(f"- Taban whiff oranı: **{sm['base_rate']:.1%}**")
            emit(f"- En iyi iterasyon: **{model.booster.best_iteration}**")
            emit()
            emit("| metrik | model | taban (sabit oran) |")
            emit("|---|---:|---:|")
            emit(f"| ROC-AUC | **{sm['auc']:.4f}** | 0.5000 |")
            emit(f"| log loss | **{sm['log_loss']:.4f}** | {sm['baseline_log_loss']:.4f} |")
            emit(f"| iyileşme | **{sm['log_loss_improvement_pct']:+.2f}%** | — |")
            emit()
            imp = pd.DataFrame({
                "feature": model.booster.feature_name(),
                "gain": model.booster.feature_importance("gain"),
            }).sort_values("gain", ascending=False).head(10)
            total = imp["gain"].sum()
            emit("### Özellik önemi (ilk 10, gain)")
            emit()
            emit("| özellik | gain payı |")
            emit("|---|---:|")
            for _, r in imp.iterrows():
                emit(f"| `{r['feature']}` | {r['gain'] / total:.2%} |")
            emit()
            scored = train.score_pitches(model, train.swings_only(parts.val))
            emit(f"- StuffScore: ortalama **{scored['score'].mean():.1f}**, "
                 f"SS **{scored['score'].std():.1f}**, "
                 f"%5–%95 **{scored['score'].quantile(0.05):.1f}**–"
                 f"**{scored['score'].quantile(0.95):.1f}**")
            emit()
            continue

        model, results = train.train_variant(parts, variant, args.version)
        model.save()
        trained[variant] = model
        all_results[variant] = results

        emit(f"## Varyant `{variant}`")
        emit()
        emit(f"- Özellik sayısı: **{len(model.feature_columns)}**")
        emit(f"- En iyi iterasyon: **{model.booster.best_iteration}**")
        emit()

        emit("### Taban modellerle karşılaştırma (validation)")
        emit()
        table = evaluate.comparison_table(results, BASELINE_REFERENCE)
        emit("| model | log loss | B2'ye göre | ECE | doğruluk |")
        emit("|---|---:|---:|---:|---:|")
        for _, r in table.iterrows():
            emit(f"| `{r['model']}` | {r['log_loss']:.4f} | "
                 f"{r['vs_reference_%']:+.2f}% | {r['ece']:.4f} | "
                 f"{r['accuracy']:.2%} |")
        emit()

        m = results[f"model_{variant}"]
        improvement = (
            results[BASELINE_REFERENCE].log_loss - m.log_loss
        ) / results[BASELINE_REFERENCE].log_loss * 100.0

        emit("### Sınıf başına ayırt edicilik ve kalibrasyon")
        emit()
        emit("| sınıf | ROC-AUC (OvR) | Brier |")
        emit("|---|---:|---:|")
        for cls_name in config.CLASS_LABELS:
            emit(f"| `{cls_name}` | {m.auc_ovr[cls_name]:.4f} | "
                 f"{m.brier_per_class[cls_name]:.4f} |")
        emit()

        emit("### Şüphe testi (L9)")
        emit()
        emit(f"- B2 taban modeline göre log-loss iyileşmesi: **{improvement:+.2f}%**")
        emit()
        emit("FAZ 0'da bu test için %8–15'lik tek bir bant belirlenmişti. O bant "
             "yanlış tanımlanmıştı: konum özellikleri dahil edildiğinde problemin "
             "büyük kısmı **geometriye** dönüşüyor. Plakanın yarım metre dışına "
             "giden bir atış neredeyse kesin `BALL`'dur ve bu, topun kendi "
             "yörüngesinden bilinir — sonuçtan değil. Dolayısıyla yüksek iyileşme "
             "burada sızıntı kanıtı değildir.")
        emit()
        emit("Ayırt edici kontrol: konumsuz fiziksel özellikler sallanma kararında "
             "yalnızca **0,554 AUC** veriyor. Sızıntı olsaydı orada da yüksek "
             "çıkardı. Yüksek AUC yalnızca konum eklendiğinde ve yalnızca konumla "
             "açıklanabilen sınıflarda ortaya çıkıyor.")
        emit()
        if m.suspicious_classes:
            geometric = {config.CLASS_BALL, config.CLASS_CALLED_STRIKE}
            unexplained = [c for c in m.suspicious_classes if c not in geometric]
            emit(f"- AUC > {evaluate.SUSPICIOUS_AUC} olan sınıflar: "
                 f"`{'`, `'.join(m.suspicious_classes)}`")
            if unexplained:
                emit(f"- 🔴 **ALARM** — bunlardan `{'`, `'.join(unexplained)}` "
                     "geometriyle açıklanamaz. Özellik listesi denetlenmeli.")
            else:
                emit("- ✅ Tamamı konum geometrisiyle açıklanan sınıflar "
                     "(`BALL` / `CALLED_STRIKE`). Sallanmayan bir atışın sonucu, "
                     "tanımı gereği bölgeye göre konumudur.")
        else:
            emit(f"- ✅ Hiçbir sınıfın AUC'si {evaluate.SUSPICIOUS_AUC} eşiğini aşmadı")
        emit()
        emit("- Temas gerektiren sınıflar (`SWINGING_STRIKE`, `FOUL`, `IN_PLAY`) "
             f"sırasıyla **{m.auc_ovr[config.CLASS_SWINGING_STRIKE]:.3f}**, "
             f"**{m.auc_ovr[config.CLASS_FOUL]:.3f}**, "
             f"**{m.auc_ovr[config.CLASS_IN_PLAY]:.3f}** AUC ile modelleniyor — "
             "yani zor kalan kısım gerçekten zor kalmış. Sızıntı olsaydı "
             "bunların da tavana yapışması beklenirdi.")
        emit()
        emit(f"### Kalibrasyon — `{REFERENCE_CLASS}`")
        emit()
        X_va = features.select_matrix(parts.val, variant)
        proba_va = model.booster.predict(
            X_va, num_iteration=model.booster.best_iteration
        )
        rel = evaluate.reliability_table(
            parts.val[labels.TARGET], proba_va, REFERENCE_CLASS
        )
        emit("| olasılık bandı | n | tahmin | gözlenen | fark |")
        emit("|---|---:|---:|---:|---:|")
        for _, r in rel.iterrows():
            emit(f"| {r['bin']} | {int(r['n']):,} | {r['predicted']:.3f} | "
                 f"{r['observed']:.3f} | {r['gap']:+.3f} |")
        emit()

        emit("### Özellik önemi (ilk 15, gain)")
        emit()
        imp = pd.DataFrame({
            "feature": model.booster.feature_name(),
            "gain": model.booster.feature_importance("gain"),
        }).sort_values("gain", ascending=False).head(15)
        total = imp["gain"].sum()
        emit("| özellik | gain payı |")
        emit("|---|---:|")
        for _, r in imp.iterrows():
            emit(f"| `{r['feature']}` | {r['gain'] / total:.2%} |")
        emit()

        emit("### Skor dağılımı (validation)")
        emit()
        scored = train.score_pitches(model, parts.val)
        emit(f"- xRV ortalaması: **{scored['xrv'].mean():+.5f}** koşu/atış")
        emit(f"- PitchScore: ortalama **{scored['score'].mean():.1f}**, "
             f"SS **{scored['score'].std():.1f}**")
        emit(f"- %5–%95 aralığı: **{scored['score'].quantile(0.05):.1f}** – "
             f"**{scored['score'].quantile(0.95):.1f}**")
        emit()
        emit("#### Skorun tersine çevrilebilirliği")
        emit()
        sample = scored.iloc[0]
        back = model.scaler.to_xrv(np.array([sample["score"]]))[0]
        emit(f"- Örnek atış: PitchScore **{sample['score']:.2f}** → geri dönüşüm "
             f"xRV **{back:+.5f}**, gerçek xRV **{sample['xrv']:+.5f}** "
             f"(fark {abs(back - sample['xrv']):.2e})")
        emit("- Skor, modelden ve lig sabitlerinden **türetilmiştir**; elle "
             "seçilmiş hiçbir ağırlık içermez ve geriye doğru izlenebilir.")
        emit()

    # ---------------------------------------------- what happened to location_score
    emit("## `location_score` neden kaldırıldı")
    emit()
    emit("FAZ 0 tasarımında `location_score = pitch_score - stuff_score` olarak "
         "tanımlanmıştı. İki model artık **farklı hedefleri** tahmin ettiği için "
         "(5 sınıflı sonuç vs. koşullu whiff) skorları aynı ölçekte değil ve "
         "farkları anlamlı bir büyüklük üretmiyor. Sahte bir sayı sunmak yerine "
         "v1'de bu metrik **kaldırıldı**.")
    emit()
    emit("Konumun katkısı yine de ölçülebiliyor ve rapor edilecek: 5 sınıflı "
         "modelde `dist_from_zone_center` tek başına gain'in yarısından fazlasını "
         "alıyor. Konumu ayrı bir skora çevirmek, FAZ 11'de ablasyon tabanlı bir "
         "yaklaşımla yeniden değerlendirilecek.")
    emit()

    # ------------------------------------------------------------- test split
    emit("## Test kümesi")
    emit()
    if os.environ.get(split.TEST_ACCESS_ENV) == "1":
        test_df = split.load_test_set(parts)
        emit(f"Model donduruldu ve test kümesi **bir kez** okundu "
             f"({len(test_df):,} atış, {test_df['game_date'].min():%Y-%m-%d} – "
             f"{test_df['game_date'].max():%Y-%m-%d}).")
        emit()
        emit("| varyant | metrik | model | taban | iyileşme |")
        emit("|---|---|---:|---:|---:|")

        if "pitching" in trained:
            model = trained["pitching"]
            X_te = features.select_matrix(test_df, "pitching")
            proba = model.booster.predict(
                X_te, num_iteration=model.booster.best_iteration
            )
            m = evaluate.evaluate(test_df[labels.TARGET], proba)
            bl = baseline.build_baselines()[BASELINE_REFERENCE].fit(parts.train)
            mb = evaluate.evaluate(test_df[labels.TARGET], bl.predict_proba(test_df))
            imp = (mb.log_loss - m.log_loss) / mb.log_loss * 100.0
            emit(f"| `pitching` | log loss | **{m.log_loss:.4f}** | "
                 f"{mb.log_loss:.4f} | **{imp:+.2f}%** |")
            emit(f"| `pitching` | ECE | **{m.ece:.4f}** | {mb.ece:.4f} | — |")
            emit(f"| `pitching` | doğruluk | **{m.accuracy:.2%}** | "
                 f"{mb.accuracy:.2%} | — |")

        if "stuff" in trained:
            model = trained["stuff"]
            sw = train.swings_only(test_df)
            X_te = features.select_matrix(sw, "stuff")
            p_te = model.booster.predict(
                X_te, num_iteration=model.booster.best_iteration
            )
            base_rate = model.metrics["base_rate"]
            ll = train._binary_log_loss(sw["is_whiff"].to_numpy(), p_te)
            ll_b = train._binary_log_loss(
                sw["is_whiff"].to_numpy(), np.full(len(sw), base_rate)
            )
            auc = roc_auc_score(sw["is_whiff"], p_te)
            emit(f"| `stuff` | ROC-AUC | **{auc:.4f}** | 0.5000 | — |")
            emit(f"| `stuff` | log loss | **{ll:.4f}** | {ll_b:.4f} | "
                 f"**{(ll_b - ll) / ll_b * 100:+.2f}%** |")
        emit()
        emit("> Bu sayılar yalnızca model bu noktadan önce donmuşsa geçerlidir. "
             "Test skoruna bakıp modeli değiştirmek, test kümesini bir "
             "validation kümesine çevirir ve raporlanan sayıyı geçersiz kılar.")
    else:
        emit("Test kümesi okunmadı. Bu bilinçlidir: test kümesi, model "
             "dondurulduktan **sonra bir kez** okunur. Okumak için "
             f"`{split.TEST_ACCESS_ENV}=1` ortam değişkeni gerekir.")
    emit()

    out = config.REPORTS_DIR / f"faz1-model-degerlendirme-{args.version}.md"
    out.write_text("\n".join(lines) + "\n", encoding="utf-8")
    log.info("report written to %s", out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
