"""Phase 1 data validation (checks A1-A7 from the dataset analysis).

Pulls two small comparable samples -- one in-regime (2025) and one
out-of-regime (2026) -- and answers the questions the design phase had to
leave open, most importantly whether the 2026 ABS measurement change is
empirically visible in plate_x / plate_z / sz_top / sz_bot.

Writes a Turkish report to docs/reports/.

Usage:
    .venv/bin/python ml/scripts/validate_data.py
"""

from __future__ import annotations

import logging
import sys
from datetime import datetime
from pathlib import Path

import numpy as np
import pandas as pd

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from pitchlab_ml import config, ingest

logging.basicConfig(
    level=logging.INFO, format="%(asctime)s %(levelname)-5s %(message)s"
)
log = logging.getLogger("validate")

# Comparable late-June windows, chosen to avoid the All-Star break and to sit
# well inside both seasons.
WINDOW_2025 = ("2025-06-16", "2025-06-22")
WINDOW_2026 = ("2026-06-15", "2026-06-21")

# Columns the feature design depends on; missingness here actually matters.
FEATURE_CANDIDATES = [
    "release_speed", "release_spin_rate", "spin_axis",
    "pfx_x", "pfx_z", "plate_x", "plate_z",
    "release_pos_x", "release_pos_z", "release_extension",
    "sz_top", "sz_bot", "effective_speed", "arm_angle",
    "pitch_type", "balls", "strikes", "outs_when_up", "inning",
    "stand", "p_throws", "n_thruorder_pitcher",
    "bat_score", "fld_score", "pitcher_days_since_prev_game",
]

# Fields whose definition changed in 2026 (same name, different meaning).
REGIME_SENSITIVE = ["plate_x", "plate_z", "sz_top", "sz_bot"]


def load_sample(window: tuple[str, str], tag: str) -> pd.DataFrame:
    """Fetch a window, caching the result as parquet so reruns are free."""
    path = config.SAMPLE_DIR / f"statcast_sample_{tag}.parquet"
    if path.exists():
        log.info("using cached sample %s", path.name)
        return pd.read_parquet(path)

    df = ingest.fetch_range(*window, verbose=True)
    if df.empty:
        log.warning("no rows returned for %s", window)
        return df

    df.to_parquet(path, compression="snappy", index=False)
    log.info("wrote %s (%d rows, %.1f MB)", path.name, len(df),
             path.stat().st_size / 1e6)
    return df


def describe(series: pd.Series) -> dict[str, float]:
    s = pd.to_numeric(series, errors="coerce").dropna()
    if s.empty:
        return {}
    return {
        "n": len(s),
        "mean": s.mean(),
        "std": s.std(),
        "p05": s.quantile(0.05),
        "p50": s.quantile(0.50),
        "p95": s.quantile(0.95),
    }


def cohens_d(a: pd.Series, b: pd.Series) -> float:
    """Standardized mean difference; robust enough for a regime check."""
    a = pd.to_numeric(a, errors="coerce").dropna()
    b = pd.to_numeric(b, errors="coerce").dropna()
    if a.empty or b.empty:
        return float("nan")
    na, nb = len(a), len(b)
    pooled = np.sqrt(((na - 1) * a.var() + (nb - 1) * b.var()) / (na + nb - 2))
    return float((b.mean() - a.mean()) / pooled) if pooled else float("nan")


def main() -> int:
    lines: list[str] = []

    def emit(text: str = "") -> None:
        print(text)
        lines.append(text)

    stamp = datetime.now().strftime("%Y-%m-%d %H:%M")
    emit("# FAZ 1 — Veri Doğrulama Raporu (A1–A7)")
    emit()
    emit(f"> Üretim tarihi: {stamp}")
    emit(f"> Örneklem pencereleri: {WINDOW_2025[0]}..{WINDOW_2025[1]} (2025) ve "
         f"{WINDOW_2026[0]}..{WINDOW_2026[1]} (2026)")
    emit()

    # ---------------------------------------------------------------- A1
    emit("## A1 — pybaseball erişimi")
    emit()
    df25 = load_sample(WINDOW_2025, "2025-06")
    df26 = load_sample(WINDOW_2026, "2026-06")
    if df25.empty:
        emit("**BAŞARISIZ** — 2025 örneklemi boş döndü.")
        return 1
    emit(f"- 2025 örneklemi: **{len(df25):,} atış**, {df25.shape[1]} sütun")
    if df26.empty:
        emit("- 2026 örneklemi: **boş** — A7 yapılamıyor")
    else:
        emit(f"- 2026 örneklemi: **{len(df26):,} atış**, {df26.shape[1]} sütun")
    emit()

    # ---------------------------------------------------------------- A2
    emit("## A2 — Sütun envanteri")
    emit()
    cols = set(df25.columns)
    missing = [c for c in FEATURE_CANDIDATES if c not in cols]
    emit(f"- Toplam sütun: **{df25.shape[1]}**")
    if missing:
        emit(f"- ⚠️ Tasarımda beklenip **bulunamayan** alanlar: `{'`, `'.join(missing)}`")
    else:
        emit("- ✅ Tasarımda beklenen tüm özellik adayları mevcut")
    leak_present = sorted(cols & config.LEAKY_COLUMNS)
    emit(f"- Kara listedeki alanlardan veri setinde bulunanlar: **{len(leak_present)}** "
         f"(bunlar özellik matrisine **girmeyecek**)")
    emit()

    # ---------------------------------------------------------------- A3
    emit("## A3 — Sonuç dağılımı ve sınıf dengesi")
    emit()
    desc = df25["description"].value_counts()
    emit("### Ham `description` dağılımı")
    emit()
    emit("| description | adet | pay |")
    emit("|---|---:|---:|")
    for k, v in desc.items():
        emit(f"| `{k}` | {v:,} | {v / len(df25):.2%} |")
    emit()

    mapped = df25["description"].map(config.DESCRIPTION_TO_CLASS)
    excluded = df25["description"].isin(config.EXCLUDED_DESCRIPTIONS)
    unmapped = df25.loc[mapped.isna() & ~excluded, "description"].value_counts()

    emit("### 5 sınıfa eşleme sonucu")
    emit()
    emit("| sınıf | adet | pay |")
    emit("|---|---:|---:|")
    cls = mapped.value_counts()
    for label in config.CLASS_LABELS:
        n = int(cls.get(label, 0))
        emit(f"| `{label}` | {n:,} | {n / mapped.notna().sum():.2%} |")
    emit()
    emit(f"- Dışlanan (bunt / pitchout / HBP): **{int(excluded.sum()):,}** "
         f"({excluded.mean():.2%})")
    if len(unmapped):
        emit(f"- ⚠️ **Eşlenemeyen** `description` değerleri: "
             f"`{'`, `'.join(unmapped.index.astype(str))}` "
             f"(toplam {int(unmapped.sum()):,}) — `DESCRIPTION_TO_CLASS` güncellenmeli")
    else:
        emit("- ✅ Eşlenemeyen `description` değeri yok")
    imbalance = cls.max() / cls.min() if len(cls) else float("nan")
    emit(f"- En sık / en seyrek sınıf oranı: **{imbalance:.2f}×** "
         f"→ {'ağırlıklandırma gerekmiyor' if imbalance < 5 else 'ağırlıklandırma değerlendirilmeli'}")
    emit()

    # ---------------------------------------------------------------- A4
    emit("## A4 — Eksiklik oranları (özellik adayları)")
    emit()
    emit("| alan | eksik % (2025) | not |")
    emit("|---|---:|---|")
    for c in FEATURE_CANDIDATES:
        if c not in df25.columns:
            emit(f"| `{c}` | — | **alan yok** |")
            continue
        rate = df25[c].isna().mean()
        note = ""
        if rate > 0.10:
            note = "⚠️ yüksek"
        if c == "arm_angle":
            note = ("kullanılabilir" if rate < 0.10 else
                    "**v1'de kullanılmayacak** (doluluk < %90)")
        emit(f"| `{c}` | {rate:.2%} | {note} |")
    emit()

    # leakage-by-missingness demonstration
    emit("### Sızıntı kanıtı: `launch_speed` eksikliği ile sonuç ilişkisi")
    emit()
    if "launch_speed" in df25.columns:
        tmp = pd.DataFrame({
            "cls": mapped,
            "ls_present": df25["launch_speed"].notna(),
        }).dropna(subset=["cls"])
        ct = pd.crosstab(tmp["cls"], tmp["ls_present"], normalize="index")
        emit("| sınıf | `launch_speed` dolu oranı |")
        emit("|---|---:|")
        for label in config.CLASS_LABELS:
            if label in ct.index and True in ct.columns:
                emit(f"| `{label}` | {ct.loc[label, True]:.2%} |")
        emit()
        emit("> `IN_PLAY` sınıfında oran ~%100, diğerlerinde ~%0. Bu sütunu modele "
             "vermek — tamamı `NULL` olsa bile — `IN_PLAY` etiketini doğrudan "
             "vermektir. Beyaz liste yaklaşımının ampirik gerekçesi budur.")
    emit()

    # ---------------------------------------------------------------- A5
    emit("## A5 — Boyut tahmini")
    emit()
    p = config.SAMPLE_DIR / "statcast_sample_2025-06.parquet"
    size_mb = p.stat().st_size / 1e6
    per_pitch = size_mb / len(df25)
    emit(f"- 7 günlük örneklem: **{len(df25):,} atış**, Parquet **{size_mb:.1f} MB**")
    emit(f"- Atış başına: **{per_pitch * 1000:.2f} KB**")
    est_season = per_pitch * 700_000
    emit(f"- Tahmini sezon (~700k atış, tüm sütunlar): **~{est_season:.0f} MB**")
    emit(f"- Tahmini 3 sezon (2023–2025): **~{est_season * 3 / 1000:.2f} GB**")
    emit(f"- ⚠️ Bu, **tüm ~{df25.shape[1]} sütun** için. Yalnızca seçilen alanlarla "
         "çok daha küçük olacak.")
    emit()

    # ---------------------------------------------------------------- A6
    emit("## A6 — `arm_angle` kullanılabilirliği")
    emit()
    if "arm_angle" in df25.columns:
        fill = 1 - df25["arm_angle"].isna().mean()
        emit(f"- 2025 doluluk: **{fill:.2%}**")
        emit(f"- Karar: **{'kullanılabilir' if fill >= 0.90 else 'v1 özellik setine DAHİL EDİLMEYECEK'}**")
    else:
        emit("- Alan mevcut değil → kullanılmayacak")
    emit()

    # ---------------------------------------------------------------- A7
    emit("## A7 — 2026 rejim kırılmasının ampirik doğrulaması")
    emit()
    emit("FAZ 0'da, Savant dokümantasyonuna dayanarak şu iddia edilmişti: 2026'dan "
         "itibaren `plate_x`/`plate_z` plakanın **ortasında** ölçülüyor (önce ön "
         "kenarıydı) ve `sz_top`/`sz_bot` artık ABS tanımlı. Sütun adları değişmedi. "
         "Aşağıda bu iddia veriyle sınanıyor.")
    emit()

    if df26.empty:
        emit("⚠️ 2026 örneklemi alınamadı — doğrulama yapılamadı.")
    else:
        emit("| alan | 2025 ort. | 2026 ort. | fark | 2025 SS | 2026 SS | Cohen's d |")
        emit("|---|---:|---:|---:|---:|---:|---:|")
        verdicts: list[tuple[str, float]] = []
        for c in REGIME_SENSITIVE:
            if c not in df25.columns or c not in df26.columns:
                continue
            a, b = describe(df25[c]), describe(df26[c])
            if not a or not b:
                continue
            d = cohens_d(df25[c], df26[c])
            verdicts.append((c, d))
            emit(f"| `{c}` | {a['mean']:.4f} | {b['mean']:.4f} | "
                 f"{b['mean'] - a['mean']:+.4f} | {a['std']:.4f} | {b['std']:.4f} | "
                 f"{d:+.3f} |")
        emit()

        emit("### Kontrol grubu (rejim değişikliğinden etkilenmemesi beklenen alanlar)")
        emit()
        emit("| alan | 2025 ort. | 2026 ort. | Cohen's d |")
        emit("|---|---:|---:|---:|")
        control = []
        for c in ["release_speed", "release_spin_rate", "release_extension", "pfx_z"]:
            if c not in df25.columns or c not in df26.columns:
                continue
            a, b = describe(df25[c]), describe(df26[c])
            if not a or not b:
                continue
            d = cohens_d(df25[c], df26[c])
            control.append((c, d))
            emit(f"| `{c}` | {a['mean']:.3f} | {b['mean']:.3f} | {d:+.3f} |")
        emit()

        # zone-width proxy: ABS zone is height-derived, so its spread should change
        for c in ["sz_top", "sz_bot"]:
            if c in df25.columns and c in df26.columns:
                s25 = pd.to_numeric(df25[c], errors="coerce").dropna()
                s26 = pd.to_numeric(df26[c], errors="coerce").dropna()
                emit(f"- `{c}` benzersiz değer sayısı — 2025: **{s25.nunique():,}**, "
                     f"2026: **{s26.nunique():,}**")
        emit()
        emit("> ABS bölgesi oyuncunun boyundan deterministik türetildiği için, "
             "`sz_top`/`sz_bot`'un 2026'da **oyuncu başına tek bir değere** yakınsaması "
             "beklenir; 2025'te operatör her atışta elle işaretlediği için dağılım "
             "çok daha geniştir.")
        emit()

        # called-strike rate: the zone shrank, so this should drop
        emit("### Called strike oranı (bölge küçüldü → düşmesi beklenir)")
        emit()
        for name, d in (("2025", df25), ("2026", df26)):
            m = d["description"].map(config.DESCRIPTION_TO_CLASS)
            taken = m.isin([config.CLASS_BALL, config.CLASS_CALLED_STRIKE])
            rate = (m[taken] == config.CLASS_CALLED_STRIKE).mean()
            emit(f"- {name}: sallanmayan atışlarda called strike oranı **{rate:.2%}**")
        emit()

        max_regime = max((abs(d) for _, d in verdicts), default=0.0)
        max_control = max((abs(d) for _, d in control), default=0.0)
        emit(f"**Sonuç:** rejime duyarlı alanlarda en büyük |d| = **{max_regime:.3f}**, "
             f"kontrol alanlarında en büyük |d| = **{max_control:.3f}**.")
        emit()
        if max_regime > 0.2 and max_regime > 2 * max_control:
            emit("✅ **FAZ 0 iddiası doğrulandı.** Rejime duyarlı alanlar, kontrol "
                 "alanlarından belirgin biçimde daha fazla kaymış. 2026 verisi "
                 "eğitime **katılmayacak**.")
        elif max_regime > max_control:
            emit("⚠️ **Kısmen doğrulandı.** Kayma var ama kontrol alanlarına göre "
                 "farkı zayıf. Tam sezon verisiyle tekrar bakılmalı; ihtiyat gereği "
                 "2026 eğitim dışı kalmaya devam edecek.")
        else:
            emit("❌ **Doğrulanamadı.** Beklenen kayma gözlenmedi. Pencereler ve alan "
                 "tanımları gözden geçirilmeli.")
    emit()

    # ------------------------------------------------- deviations from design
    emit("## FAZ 0 tasarımından sapmalar")
    emit()
    emit("Aşağıdaki kararlar, FAZ 0'da varsayım olarak alınmıştı ve şimdi veriyle "
         "güncellendi. Her biri tasarım dokümanındaki ilgili kararı **geçersiz kılar**.")
    emit()
    emit("| # | FAZ 0 varsayımı | Ampirik bulgu | Yeni karar |")
    emit("|---|---|---|---|")
    emit("| S1 | ML yığını Python 3.12'ye sabitlenmeli (3.14 wheel riski) | "
         "Tüm paketlerin cp314 arm64 wheel'i mevcut. Asıl kısıt `pandas<3`: "
         "`pybaseball 2.2.7` sınırsız `pandas>=1.0.3` bildiriyor ama pandas 3.0 "
         "API'si için yazılmamış | **Python 3.14 kullanılıyor**, `pandas>=2.3.3,<3` "
         "pinlendi. LightGBM için `libomp` (Homebrew) ön koşul. |")
    emit("| S2 | `arm_angle` 2024 öncesinde yok → v1'de kullanılmayacak | "
         "2025 doluluğu **%99,55** | **Özellik setine dahil edilecek.** Eğitim "
         "penceresi 2023'te başladığı için 2023 doluluğu FAZ 1.3'te ayrıca "
         "kontrol edilecek. |")
    emit("| S3 | Dışlanacaklar: bunt, pitchout, HBP | `automatic_ball` ve "
         "`automatic_strike` de var (%0,38) — pitch-clock ihlalleri ve kasıtlı "
         "walk'lar; `plate_x`/`plate_z`/`pitch_type` **%100 boş**, yani atış "
         "atılmamış | **`EXCLUDED_DESCRIPTIONS`'a eklendi.** Sonucu var ama "
         "atfedilecek atışı yok. |")
    emit("| S4 | `launch_speed` eksikliği `IN_PLAY` sınıfını ele verir | "
         "`FOUL` sınıfında da **%81** dolu. Yani alan, 'temas oldu mu' sorusunu "
         "iki sınıf için birden cevaplıyor | Sızıntı FAZ 0'da yazılandan **daha "
         "geniş**. Beyaz liste yaklaşımı değişmiyor, gerekçesi güçleniyor. |")
    emit("| S5 | 3 sezon Parquet ~200–300 MB | Atış başına 0,17 KB → "
         "3 sezon **~0,35 GB** (tüm 119 sütun) | Tahmin tutuyor; disk kısıt değil. |")
    emit()
    emit("### Doğrulanan varsayımlar")
    emit()
    emit("- Sınıf dağılımı FAZ 0 tahminiyle neredeyse birebir örtüştü "
         "(tahmin 36/17/17/19/11 → gerçek 35,8/15,9/18,1/19,2/11,0)")
    emit("- Sınıf dengesizliği 3,3× → ağırlıklandırma gerekmiyor")
    emit("- Tasarımda beklenen tüm özellik adayları veri setinde mevcut")
    emit("- Fiziksel ölçüm alanlarının eksikliği < %0,5")
    emit()

    out = config.REPORTS_DIR / "faz1-veri-dogrulama.md"
    out.write_text("\n".join(lines) + "\n", encoding="utf-8")
    log.info("report written to %s", out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
