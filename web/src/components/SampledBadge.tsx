//
// SampledBadge.tsx
// Saying out loud that a number on this page is an estimate.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import type { Sampling } from "../api/types";
import { compact } from "../lib/format";
import { t } from "../lib/i18n";

interface Props {
	/** What the response said about sampling, or undefined when the numbers are
	 *  exact. */
	sampling: Sampling | undefined;

	/** Whether the reader has asked for exactness. It is separate from the
	 *  sampling object because an exact answer carries no sampling at all, and
	 *  the reader still needs to see that the switch they pressed is on. */
	exact: boolean;

	onExact: (exact: boolean) => void;
}

/**
 * SampledBadge is the whole honesty mechanism, on the screen.
 *
 * A sampled figure that looks exact is worse than a slow exact one, because
 * somebody will make a decision on it — so this sits above the numbers rather
 * than in a tooltip on one of them, says what fraction was read and how much
 * data was behind the question, and offers the exact answer in the same breath.
 * An escape hatch nobody is told about is not an escape hatch.
 */
export function SampledBadge({ sampling, exact, onExact }: Props) {
	if (!sampling && !exact) return null;

	if (!sampling) {
		return (
			<Banner>
				<span className="text-body">{t("dashboard.sampled.exact_note")}</span>
				<Action onClick={() => onExact(false)}>{t("dashboard.sampled.estimate_action")}</Action>
			</Banner>
		);
	}

	// The rate reads as a percentage because that is how somebody thinks about
	// it — "a tenth of visitors", not "0.1".
	const percent = Math.round(sampling.rate * 1000) / 10;

	const explanation =
		sampling.reason === "automatic"
			? t("dashboard.sampled.automatic", {
					percent,
					rows: compact(sampling.estimated_rows ?? 0),
				})
			: t("dashboard.sampled.requested", { percent });

	return (
		<Banner>
			{/* Outlined rather than filled: warn and warn-ink are two shades of
			    the same amber, so a filled pill would be amber text on amber in
			    dark mode. The outline reads in both. */}
			<span className="rounded-sm border border-warn px-1.5 py-0.5 text-[10px] font-semibold tracking-wide text-warn uppercase">
				{t("dashboard.sampled.badge")}
			</span>

			<span className="text-body">{explanation}</span>

			{sampling.reason === "automatic" && (
				<Action onClick={() => onExact(true)}>{t("dashboard.sampled.exact_action")}</Action>
			)}
		</Banner>
	);
}

/** Banner is the strip both states share, so switching between them does not
 *  move the numbers underneath. */
function Banner({ children }: { children: React.ReactNode }) {
	return (
		<div
			role="status"
			className="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-line bg-subtle px-5 py-2 text-xs"
		>
			{children}
		</div>
	);
}

/** Action is the inline link that changes which answer is being shown. */
function Action({ children, onClick }: { children: React.ReactNode; onClick: () => void }) {
	return (
		<button
			type="button"
			onClick={onClick}
			className="font-medium text-accent underline underline-offset-2 transition-colors duration-150 ease-[var(--ease-ui)] hover:text-accent-hover"
		>
			{children}
		</button>
	);
}
