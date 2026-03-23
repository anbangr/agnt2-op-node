#!/usr/bin/env python3
"""Render a human-readable single-block SDM replay report from JSONL output."""

import argparse
import json
import sys
import urllib.request


def read_records(path):
    records = []
    if path == "-":
        f = sys.stdin
        close = False
    else:
        f = open(path)
        close = True
    try:
        for line in f:
            line = line.strip()
            if not line:
                continue
            records.append(json.loads(line))
    finally:
        if close:
            f.close()
    return records


def ratio(numerator, denominator):
    return numerator / denominator if denominator else 0.0


def format_int(value):
    if value is None or value == "-":
        return "-"
    return f"{int(value):,}"


def format_pct(value):
    if value is None or value == "-":
        return "-"
    return f"{float(value):.2%}"


def bool_text(value):
    return "yes" if value else "no"


def markdown_table(headers, rows):
    out = []
    out.append("| " + " | ".join(headers) + " |")
    out.append("| " + " | ".join(["---"] * len(headers)) + " |")
    for row in rows:
        out.append("| " + " | ".join(str(cell) for cell in row) + " |")
    return "\n".join(out)


def rpc_call(rpc_url, method, params):
    payload = json.dumps({"jsonrpc": "2.0", "method": method, "params": params, "id": 1}).encode()
    req = urllib.request.Request(
        rpc_url,
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = json.loads(resp.read().decode())
    if body.get("error"):
        raise RuntimeError(f"RPC error calling {method}: {body['error']}")
    return body.get("result")


def fetch_block(rpc_url, block_num):
    return rpc_call(rpc_url, "eth_getBlockByNumber", [hex(block_num), True])


def tx_type_hex(value):
    if value is None:
        return "-"
    if isinstance(value, str):
        return value.lower()
    return f"0x{int(value):x}"


def tx_role(source_tx, tx_record):
    if source_tx and tx_type_hex(source_tx.get("type")) == "0x7d":
        return "sdm"
    if tx_record and tx_record.get("is_deposit_tx"):
        return "deposit"
    return "user"


def build_transaction_rows(block_record, tx_records, source_block):
    rows = []
    txs_by_index = {rec["tx_index"]: rec for rec in tx_records}

    if source_block and source_block.get("transactions"):
        source_txs = source_block["transactions"]
        for idx, source_tx in enumerate(source_txs):
            tx_record = txs_by_index.get(idx)
            notes = []
            replay_idx = "-"
            gas_used = "-"
            replay_refund = "-"
            receipt_refund = "-"
            payload_refund = "-"
            refund_ratio = "-"
            effective_gas = "-"

            if tx_record is not None:
                replay_idx = tx_record.get("replay_tx_index", "-")
                gas_used = format_int(tx_record.get("gas_used"))
                replay_refund = format_int(tx_record.get("op_gas_refund_replay"))
                receipt_refund = format_int(tx_record.get("op_gas_refund_receipt"))
                payload_refund = format_int(tx_record.get("op_gas_refund_payload"))
                refund_ratio = format_pct(tx_record.get("refund_ratio"))
                effective_gas = format_int(tx_record.get("effective_gas"))
                if tx_record.get("mismatch"):
                    notes.append("mismatch")
            elif tx_type_hex(source_tx.get("type")) == "0x7d":
                notes.append("source SDM tx; excluded from replay tx rows")
            else:
                notes.append("missing replay tx row")

            rows.append(
                [
                    idx,
                    replay_idx,
                    source_tx.get("hash", tx_record.get("tx_hash") if tx_record else "-"),
                    tx_type_hex(source_tx.get("type") if source_tx else tx_record.get("tx_type") if tx_record else None),
                    tx_role(source_tx, tx_record),
                    gas_used,
                    replay_refund,
                    receipt_refund,
                    payload_refund,
                    refund_ratio,
                    effective_gas,
                    "; ".join(notes) if notes else "-",
                ]
            )
        return rows

    for tx_record in sorted(tx_records, key=lambda rec: rec["tx_index"]):
        notes = []
        if tx_record.get("mismatch"):
            notes.append("mismatch")
        rows.append(
            [
                tx_record.get("tx_index", "-"),
                tx_record.get("replay_tx_index", "-"),
                tx_record.get("tx_hash", "-"),
                tx_type_hex(tx_record.get("tx_type")),
                "deposit" if tx_record.get("is_deposit_tx") else "user",
                format_int(tx_record.get("gas_used")),
                format_int(tx_record.get("op_gas_refund_replay")),
                format_int(tx_record.get("op_gas_refund_receipt")),
                format_int(tx_record.get("op_gas_refund_payload")),
                format_pct(tx_record.get("refund_ratio")),
                format_int(tx_record.get("effective_gas")),
                "; ".join(notes) if notes else "-",
            ]
        )

    if block_record.get("sdm_tx_present") and block_record.get("tx_count_total", 0) > len(tx_records):
        rows.append(["-", "-", "(source SDM tx not fetched)", "0x7d", "sdm", "-", "-", "-", "-", "-", "-", "excluded from replay tx rows"])
    return rows


def build_mismatch_rows(records):
    rows = []
    for rec in records:
        rows.append(
            [
                rec.get("category", "-"),
                rec.get("tx_index", "-"),
                format_int(rec.get("expected")) if "expected" in rec else "-",
                format_int(rec.get("actual")) if "actual" in rec else "-",
                rec.get("message", "-"),
            ]
        )
    return rows


def render_report(records, rpc_url=None):
    run_config = None
    block_records = []
    tx_records = []
    mismatches = []
    summary = None

    for rec in records:
        rec_type = rec.get("type")
        if rec_type == "run_config":
            run_config = rec
        elif rec_type == "block":
            block_records.append(rec)
        elif rec_type == "tx":
            tx_records.append(rec)
        elif rec_type == "mismatch":
            mismatches.append(rec)
        elif rec_type == "summary":
            summary = rec

    if len(block_records) != 1:
        raise ValueError(f"expected exactly one block record, found {len(block_records)}")

    block_record = block_records[0]
    block_num = block_record["block_num"]
    source_block = None
    source_block_error = None
    if rpc_url:
        try:
            source_block = fetch_block(rpc_url, block_num)
        except Exception as exc:  # noqa: BLE001
            source_block_error = str(exc)

    block_refund_ratio = block_record.get(
        "block_refund_ratio",
        ratio(block_record.get("replay_refund_total", 0), block_record.get("block_gas_used", 0)),
    )

    lines = []
    lines.append(f"# SDM Replay Block Inspect: {block_num}")
    lines.append("")

    if run_config:
        lines.append("## Replay Context")
        context_rows = [
            ["RPC", run_config.get("rpc", "-")],
            ["Replay mode", run_config.get("replay_mode", "-")],
            ["Chain ID", run_config.get("chain_id", "-")],
            ["Compare payload", bool_text(run_config.get("compare_payload"))],
            ["Compare RPC receipts", bool_text(run_config.get("compare_rpc_receipts"))],
            ["Summary only", bool_text(run_config.get("summary_only"))],
        ]
        lines.append(markdown_table(["Field", "Value"], context_rows))
        lines.append("")

    lines.append("## Block Summary")
    summary_rows = [
        ["Block number", block_record.get("block_num", "-")],
        ["Block hash", block_record.get("block_hash", "-")],
        ["Parent hash", block_record.get("parent_hash", "-")],
        ["Tx count total", block_record.get("tx_count_total", "-")],
        ["Tx count user", block_record.get("tx_count_user", "-")],
        ["SDM tx present", bool_text(block_record.get("sdm_tx_present"))],
        ["SDM payload entries", block_record.get("sdm_payload_entry_count", "-")],
        ["Block gas used", format_int(block_record.get("block_gas_used"))],
        ["Replay refund total", format_int(block_record.get("replay_refund_total"))],
        ["Block refund ratio", format_pct(block_refund_ratio)],
        ["Block effective gas", format_int(block_record.get("block_effective_gas"))],
        ["Node receipt refund total", format_int(block_record.get("node_receipt_refund_total"))],
        ["Payload refund total", format_int(block_record.get("payload_refund_total"))],
        ["Mismatch count", block_record.get("mismatch_count", "-")],
    ]
    lines.append(markdown_table(["Field", "Value"], summary_rows))
    lines.append("")
    lines.append(
        f"Reconciliation: {format_int(block_record.get('block_effective_gas'))} effective gas + "
        f"{format_int(block_record.get('replay_refund_total'))} replay refund = "
        f"{format_int(block_record.get('block_gas_used'))} block gas used."
    )
    lines.append("")

    if summary:
        lines.append("## Range Summary Row")
        range_rows = [
            ["Blocks processed", summary.get("blocks_processed", "-")],
            ["Total gas used", format_int(summary.get("total_gas_used"))],
            ["Replay refund total", format_int(summary.get("replay_refund_total"))],
            ["Total refund ratio", format_pct(summary.get("total_refund_ratio", ratio(summary.get("replay_refund_total", 0), summary.get("total_gas_used", 0))))],
        ]
        lines.append(markdown_table(["Field", "Value"], range_rows))
        lines.append("")

    lines.append("## Transactions")
    if source_block_error:
        lines.append(f"_Warning: failed to load source block from RPC: {source_block_error}_")
        lines.append("")
    tx_rows = build_transaction_rows(block_record, tx_records, source_block)
    lines.append(
        markdown_table(
            [
                "SrcIdx",
                "ReplayIdx",
                "TxHash",
                "Type",
                "Role",
                "GasUsed",
                "ReplayRefund",
                "ReceiptRefund",
                "PayloadRefund",
                "Refund%",
                "EffectiveGas",
                "Notes",
            ],
            tx_rows,
        )
    )
    lines.append("")

    if mismatches:
        lines.append("## Mismatches")
        lines.append(markdown_table(["Category", "TxIdx", "Expected", "Actual", "Message"], build_mismatch_rows(mismatches)))
        lines.append("")

    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description="Render a single-block SDM replay inspection report")
    parser.add_argument("--input", "-i", default="-", help="Input JSONL path from sdm-replay")
    parser.add_argument("--output", "-o", default="-", help="Output markdown path (default: stdout)")
    parser.add_argument("--rpc", help="Optional RPC URL used to fetch the source block for a complete transaction list")
    args = parser.parse_args()

    report = render_report(read_records(args.input), rpc_url=args.rpc)
    if args.output == "-":
        sys.stdout.write(report)
        if not report.endswith("\n"):
            sys.stdout.write("\n")
        return

    with open(args.output, "w") as f:
        f.write(report)
        if not report.endswith("\n"):
            f.write("\n")

    print(f"Saved report to {args.output}")


if __name__ == "__main__":
    main()
