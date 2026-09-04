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

	/** The automatic plan was rejected because this query needs complete
	 * membership. The only retry offered is an explicit exact request. */
	exactFallback?: boolean;

	onExact: (exact: boolean) => void;
}

/** exactResponsesReady reports whether an exact label describes every response
 *  in a shared section. Loading keeps the answer false because useStats holds
 *  the preceding data while a replacement request is in flight. */
export function exactResponsesReady(
	requested: boolean,
	loading: boolean,
	...sampling: (Sampling | undefined)[]
): boolean {
	return requested && !loading && sampling.every((value) => value === undefined);
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
export function SampledBadge({ sampling, exact, exactFallback = false, onExact }: Props) {
	if (!sampling && !exact && !exactFallback) return null;

	if (exactFallback) {
		return (
			<Banner>
				<span className="text-body">{t("dashboard.sampled.exact_required")}</span>
				<Action onClick={() => onExact(true)}>{t("dashboard.sampled.exact_action")}</Action>
			</Banner>
		);
	}

	if (!sampling) {
		return (
			<Banner>
				<span className="text-body">{t("dashboard.sampled.exact_note")}</span>
				<Action onClick={() => onExact(false)}>{t("dashboard.sampled.estimate_action")}</Action>
			</Banner>
		);
	}

	const explanation = samplingExplanation(sampling);

	return (
		<Banner>
			{/* Outlined rather than filled: warn and warn-ink are two shades of
			    the same amber, so a filled pill would be amber text on amber in
			    dark mode. The outline reads in both. */}
			<span className="border-2 border-warn px-1.5 py-0.5 text-[10px] font-semibold tracking-wide text-warn uppercase">
				{t("dashboard.sampled.badge")}
			</span>

			<span className="text-body">{explanation}</span>

			{sampling.reason === "automatic" && (
				<Action onClick={() => onExact(true)}>{t("dashboard.sampled.exact_action")}</Action>
			)}
		</Banner>
	);
}

/**
 * SampledMark is the same admission in the space a card header has.
 *
 * Every breakdown card runs a query of its own, so a card can be sampled while
 * the strip above the graph is talking about a different query — and a card
 * that showed an estimate with nothing on it saying so is exactly the number
 * somebody screenshots. It is a label rather than a hover, because a caveat you
 * have to discover is not a caveat.
 */
export function SampledMark({ sampling }: { sampling: Sampling | undefined }) {
	if (!sampling) return null;

	const explanation = samplingExplanation(sampling);

	return (
		<span
			title={explanation}
			aria-label={explanation}
			className="border-2 border-warn px-1 py-px text-[9px] leading-none font-semibold tracking-wide text-warn uppercase"
		>
			{t("dashboard.sampled.badge")}
		</span>
	);
}

/** samplingExplanation turns the response's account of a sample into the sentence both the
 *  strip and the card label show, so the two can never describe the same rate
 *  differently. The rate reads as a percentage because that is how somebody
 *  thinks about it: "ten percent of fact rows", not "0.1". */
export function samplingExplanation(sampling: Sampling): string {
	const percent = Math.round(sampling.rate * 1000) / 10;
	let explanation: string;

	if (sampling.reason === "automatic") {
		explanation = t("dashboard.sampled.automatic", {
			percent,
			rows: compact(sampling.estimated_rows ?? 0),
		});
	} else {
		explanation = t("dashboard.sampled.requested", { percent });
	}

	if (sampling.sparse) explanation += ` ${t("dashboard.sampled.sparse")}`;
	if (sampling.zero_result) explanation += ` ${t("dashboard.sampled.zero")}`;

	return explanation;
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
			className="font-medium text-accent-ink underline underline-offset-2 transition-colors duration-150 ease-[var(--ease-ui)] hover:text-accent-hover"
		>
			{children}
		</button>
	);
}
