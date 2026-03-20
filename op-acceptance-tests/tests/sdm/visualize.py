#!/usr/bin/env python3
"""Visualize SDM JSONL output.

Supports two input shapes:
  1. Benchmark JSONL grouped by `category`
  2. `sdm-replay` JSONL grouped by `block_num`

The mode is auto-detected from the records unless `--mode` is provided.
"""

import argparse
import json
import sys
from collections import defaultdict

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.ticker as ticker


def read_records(f):
    records = []
    for line in f:
        line = line.strip()
        if not line:
            continue
        records.append(json.loads(line))
    return records


def detect_mode(records):
    for rec in records:
        if rec.get("type") == "block":
            return "replay"
        if rec.get("type") == "summary" and "category" in rec:
            return "benchmark"
    raise ValueError("Could not detect input mode from JSONL records")


def load_benchmark(records):
    summaries = {}
    tx_ratios = defaultdict(list)
    for rec in records:
        if rec["type"] == "summary" and "category" in rec:
            summaries[rec["category"]] = rec
        elif rec["type"] == "tx" and "category" in rec:
            tx_ratios[rec["category"]].append(rec.get("refund_ratio", 0.0))
    return summaries, tx_ratios


def plot_benchmark(summaries, tx_ratios, output_path):
    categories = sorted(summaries.keys())
    if not categories:
        print("No benchmark summary records found.", file=sys.stderr)
        sys.exit(1)

    fig, axes = plt.subplots(1, 3, figsize=(18, 6))
    fig.suptitle("SDM OPGas Benchmark", fontsize=14, fontweight="bold")

    colors = {
        "eoa_transfer": "#4C72B0",
        "compute_heavy": "#55A868",
        "event_emitter": "#C44E52",
        "state_bloat": "#8172B2",
        "uniswap_v2_swap": "#DD8452",
        "uniswap_v3_swap": "#937860",
        "contract_deploy": "#DA8BC3",
    }
    bar_colors = [colors.get(c, "#999999") for c in categories]

    ax = axes[0]
    mean_ratios = [summaries[c].get("mean_ratio", 0.0) for c in categories]
    bars = ax.bar(categories, mean_ratios, color=bar_colors, edgecolor="black", linewidth=0.5)
    ax.set_title("Mean Refund Ratio by Category")
    ax.set_ylabel("Refund Ratio (OPGasRefund / GasUsed)")
    ax.set_ylim(0, max(mean_ratios) * 1.3 if max(mean_ratios) > 0 else 1.0)
    ax.yaxis.set_major_formatter(ticker.PercentFormatter(xmax=1.0))
    for bar, val in zip(bars, mean_ratios):
        ax.text(
            bar.get_x() + bar.get_width() / 2,
            bar.get_height() + 0.005,
            f"{val:.1%}",
            ha="center",
            va="bottom",
            fontsize=9,
        )
    ax.tick_params(axis="x", rotation=20)

    ax = axes[1]
    import numpy as np

    x = np.arange(len(categories))
    width = 0.35
    canonical = [summaries[c]["mean_canonical"] for c in categories]
    effective = [summaries[c]["mean_effective"] for c in categories]
    ax.bar(
        x - width / 2,
        canonical,
        width,
        label="Canonical Gas",
        color="#4C72B0",
        edgecolor="black",
        linewidth=0.5,
    )
    ax.bar(
        x + width / 2,
        effective,
        width,
        label="Effective Gas",
        color="#55A868",
        edgecolor="black",
        linewidth=0.5,
    )
    ax.set_title("Mean Canonical vs Effective Gas")
    ax.set_ylabel("Gas")
    ax.set_xticks(x)
    ax.set_xticklabels(categories, rotation=20)
    ax.legend()
    ax.yaxis.set_major_formatter(ticker.FuncFormatter(lambda v, _: f"{v:,.0f}"))

    ax = axes[2]
    data = [tx_ratios.get(c, []) for c in categories]
    bp = ax.boxplot(
        data,
        tick_labels=categories,
        patch_artist=True,
        notch=True,
        medianprops=dict(color="black", linewidth=1.5),
    )
    for patch, color in zip(bp["boxes"], bar_colors):
        patch.set_facecolor(color)
        patch.set_alpha(0.7)
    ax.set_title("Refund Ratio Distribution")
    ax.set_ylabel("Refund Ratio")
    ax.yaxis.set_major_formatter(ticker.PercentFormatter(xmax=1.0))
    ax.tick_params(axis="x", rotation=20)

    plt.tight_layout()
    plt.savefig(output_path, dpi=150, bbox_inches="tight")
    print(f"Saved to {output_path}")


def load_replay(records):
    run_config = None
    summary = None
    blocks = []
    tx_ratios = defaultdict(list)

    for rec in records:
        rec_type = rec.get("type")
        if rec_type == "run_config":
            run_config = rec
        elif rec_type == "block":
            blocks.append(rec)
        elif rec_type == "summary":
            summary = rec
        elif rec_type == "tx":
            tx_ratios[rec["block_num"]].append(rec.get("refund_ratio", 0.0))

    blocks.sort(key=lambda rec: rec["block_num"])
    return run_config, summary, blocks, tx_ratios


def mean(values):
    return sum(values) / len(values) if values else 0.0


def choose_replay_bucket_size(block_count):
    if block_count > 1000:
        return 100
    if block_count > 100:
        return 10
    return 1


def choose_tx_distribution_bucket_size(block_count):
    if block_count > 100:
        return 100
    if block_count > 10:
        return 10
    return 1


def aggregate_replay(blocks, tx_ratios, bucket_size=None):
    if bucket_size is None:
        bucket_size = choose_replay_bucket_size(len(blocks))
    grouped_blocks = []
    grouped_tx_ratios = []

    for start_idx in range(0, len(blocks), bucket_size):
        chunk = blocks[start_idx : start_idx + bucket_size]
        start_block = chunk[0]["block_num"]
        end_block = chunk[-1]["block_num"]
        label = str(start_block) if start_block == end_block else f"{start_block}-{end_block}"

        grouped_blocks.append(
            {
                "label": label,
                "start_block": start_block,
                "end_block": end_block,
                "block_count": len(chunk),
                "block_gas_used": mean([rec.get("block_gas_used", 0) for rec in chunk]),
                "block_effective_gas": mean([rec.get("block_effective_gas", 0) for rec in chunk]),
                "replay_refund_total": mean([rec.get("replay_refund_total", 0) for rec in chunk]),
                "avg_refund_ratio": mean([rec.get("avg_refund_ratio", 0.0) for rec in chunk]),
                "tx_count_user": sum(rec.get("tx_count_user", 0) for rec in chunk),
                "mismatch_count": sum(rec.get("mismatch_count", 0) for rec in chunk),
                "sdm_payload_entry_count": sum(rec.get("sdm_payload_entry_count", 0) for rec in chunk),
            }
        )

        bucket_tx_ratios = []
        for rec in chunk:
            bucket_tx_ratios.extend(tx_ratios.get(rec["block_num"], []))
        grouped_tx_ratios.append(bucket_tx_ratios)

    return bucket_size, grouped_blocks, grouped_tx_ratios


def format_block_axis(ax, positions, labels, bucket_size):
    ax.set_xlabel("Block Number" if bucket_size == 1 else f"Block Range (bucket size: {bucket_size})")

    label_indices = list(range(len(labels)))
    if len(labels) <= 5:
        visible_indices = label_indices
    else:
        max_ticks = 6
        if len(labels) <= max_ticks:
            visible_indices = label_indices
        else:
            visible_indices = sorted(
                {
                    round(idx * (len(labels) - 1) / (max_ticks - 1))
                    for idx in range(max_ticks)
                }
            )

    ax.set_xticks([positions[idx] for idx in visible_indices])
    ax.set_xticklabels([labels[idx] for idx in visible_indices], rotation=20)


def plot_replay(run_config, summary, blocks, tx_ratios, output_path):
    if not blocks:
        print("No block records found.", file=sys.stderr)
        sys.exit(1)

    bucket_size, grouped_blocks, grouped_tx_ratios = aggregate_replay(blocks, tx_ratios)
    tx_bucket_size = choose_tx_distribution_bucket_size(len(blocks))
    if tx_bucket_size == bucket_size:
        tx_grouped_blocks = grouped_blocks
        tx_grouped_tx_ratios = grouped_tx_ratios
    else:
        _, tx_grouped_blocks, tx_grouped_tx_ratios = aggregate_replay(blocks, tx_ratios, tx_bucket_size)

    labels = [rec["label"] for rec in grouped_blocks]
    x_values = list(range(len(grouped_blocks)))
    gas_used = [rec["block_gas_used"] for rec in grouped_blocks]
    effective_gas = [rec["block_effective_gas"] for rec in grouped_blocks]
    refund_total = [rec["replay_refund_total"] for rec in grouped_blocks]
    avg_refund_ratio = [rec["avg_refund_ratio"] for rec in grouped_blocks]
    user_txs = [rec["tx_count_user"] for rec in grouped_blocks]
    payload_entries = [rec["sdm_payload_entry_count"] for rec in grouped_blocks]
    tx_labels = [rec["label"] for rec in tx_grouped_blocks]
    tx_x_values = list(range(len(tx_grouped_blocks)))

    fig, axes = plt.subplots(2, 2, figsize=(18, 10))
    fig.suptitle("SDM Replay Range", fontsize=14, fontweight="bold")

    subtitle = []
    if run_config:
        subtitle.append(
            f"Blocks {run_config.get('resolved_from_block')}..{run_config.get('resolved_to_block')}"
        )
        subtitle.append(f"Mode: {run_config.get('replay_mode')}")
        subtitle.append(f"Chain ID: {run_config.get('chain_id')}")
    if summary:
        subtitle.append(f"Processed: {summary.get('blocks_processed', len(blocks))}")
    if bucket_size > 1:
        subtitle.append(f"Main plots grouped: {bucket_size} blocks per bucket")
    if tx_bucket_size > 1:
        subtitle.append(f"Tx distribution grouped: {tx_bucket_size} blocks per bucket")
    fig.text(0.5, 0.95, " | ".join(subtitle), ha="center", fontsize=10)

    range_suffix = "by Block" if bucket_size == 1 else "by Range"
    tx_range_suffix = "by Block" if tx_bucket_size == 1 else f"by {tx_bucket_size}-Block Range"

    ax = axes[0][0]
    ax.plot(x_values, gas_used, marker="o", color="#4C72B0", label="Mean Block Gas Used")
    ax.plot(x_values, effective_gas, marker="o", color="#55A868", label="Mean Effective Gas")
    ax.plot(x_values, refund_total, marker="o", color="#C44E52", label="Mean Replay Refund")
    ax.set_title(f"Gas Used, Effective Gas, and Replay Refund {range_suffix}")
    ax.set_ylabel("Gas")
    ax.legend()
    ax.yaxis.set_major_formatter(ticker.FuncFormatter(lambda v, _: f"{v:,.0f}"))
    format_block_axis(ax, x_values, labels, bucket_size)

    ax = axes[0][1]
    bars = ax.bar(x_values, avg_refund_ratio, color="#8172B2", edgecolor="black", linewidth=0.5)
    ax.set_title(f"Average Refund Ratio {range_suffix}")
    ax.set_ylabel("Refund Ratio")
    ax.yaxis.set_major_formatter(ticker.PercentFormatter(xmax=1.0))
    format_block_axis(ax, x_values, labels, bucket_size)
    ymax = max(avg_refund_ratio) if avg_refund_ratio else 0.0
    ax.set_ylim(0, ymax * 1.25 if ymax > 0 else 1.0)
    if len(grouped_blocks) <= 5:
        for bar, val in zip(bars, avg_refund_ratio):
            ax.text(
                bar.get_x() + bar.get_width() / 2,
                bar.get_height() + 0.01,
                f"{val:.1%}",
                ha="center",
                va="bottom",
                fontsize=8,
            )

    ax = axes[1][0]
    has_tx_data = any(tx_grouped_tx_ratios)
    if has_tx_data:
        if tx_bucket_size > 1 or len(tx_grouped_blocks) > 20:
            non_empty = [(idx, ratios) for idx, ratios in enumerate(tx_grouped_tx_ratios) if ratios]
            bp = ax.boxplot(
                [ratios for _, ratios in non_empty],
                positions=[idx + 1 for idx, _ in non_empty],
                patch_artist=True,
                notch=False,
                widths=0.6,
                medianprops=dict(color="black", linewidth=1.2),
            )
            for patch in bp["boxes"]:
                patch.set_facecolor("#DD8452")
                patch.set_alpha(0.65)
            ax.set_xlim(0.5, len(tx_grouped_blocks) + 0.5)
            tick_positions = [idx + 1 for idx in tx_x_values]
            format_block_axis(ax, tick_positions, tx_labels, tx_bucket_size)
        else:
            for idx, ratios in enumerate(tx_grouped_tx_ratios):
                if not ratios:
                    continue
                ax.scatter([idx] * len(ratios), ratios, alpha=0.55, s=16, color="#DD8452")
            format_block_axis(ax, tx_x_values, tx_labels, tx_bucket_size)
    else:
        format_block_axis(ax, tx_x_values, tx_labels, tx_bucket_size)
        ax.text(0.5, 0.5, "No tx-level records\n(summary-only input)", ha="center", va="center", transform=ax.transAxes)
    ax.set_title(f"Tx Refund Ratio Distribution {tx_range_suffix}")
    ax.set_ylabel("Refund Ratio")
    ax.yaxis.set_major_formatter(ticker.PercentFormatter(xmax=1.0))

    ax = axes[1][1]
    width = 0.35
    import numpy as np

    x = np.arange(len(grouped_blocks))
    ax.bar(x - width / 2, user_txs, width, label="User Txs", color="#4C72B0", edgecolor="black", linewidth=0.5)
    ax.bar(
        x + width / 2,
        payload_entries,
        width,
        label="SDM Payload Entries",
        color="#55A868",
        edgecolor="black",
        linewidth=0.5,
    )
    ax.set_title(f"User Tx Count and SDM Payload Entries {range_suffix}")
    ax.set_ylabel("Count")
    format_block_axis(ax, list(x), labels, bucket_size)
    ax.legend(loc="upper right")

    plt.tight_layout(rect=(0, 0, 1, 0.93))
    plt.savefig(output_path, dpi=150, bbox_inches="tight")
    print(f"Saved to {output_path}")


def main():
    parser = argparse.ArgumentParser(description="Visualize SDM benchmark or sdm-replay results")
    parser.add_argument("--input", "-i", default="-", help="Input JSONL file (default: stdin)")
    parser.add_argument("--output", "-o", default="sdm_report.png", help="Output PNG path")
    parser.add_argument(
        "--mode",
        choices=["auto", "benchmark", "replay"],
        default="auto",
        help="Interpret input as benchmark JSONL or sdm-replay JSONL",
    )
    args = parser.parse_args()

    if args.input == "-":
        records = read_records(sys.stdin)
    else:
        with open(args.input) as f:
            records = read_records(f)

    mode = args.mode if args.mode != "auto" else detect_mode(records)
    if mode == "benchmark":
        summaries, tx_ratios = load_benchmark(records)
        plot_benchmark(summaries, tx_ratios, args.output)
        return

    run_config, summary, blocks, tx_ratios = load_replay(records)
    plot_replay(run_config, summary, blocks, tx_ratios, args.output)


if __name__ == "__main__":
    main()
