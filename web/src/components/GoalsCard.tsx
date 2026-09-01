//
// GoalsCard.tsx
// The full-width conversion report at the bottom of the dashboard.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useRef, useState } from "react";

import { QueryError, goalsReport } from "../api/client";
import type { DateRange, Filter, GoalReport } from "../api/types";
import { compact, exact, metricAxisValue, metricTitle } from "../lib/format";
import { t } from "../lib/i18n";
import { useNearViewport } from "../lib/useStats";
import { Bar, Failure, InfoDot, Spinner } from "./atoms";

interface Props {
	domain: string;
	range: DateRange;
	filters: Filter[];
	exact: boolean;
}

interface GoalsState {
	data: GoalReport | null;
	loading: boolean;
	error: string | null;
	reload: () => void;
}

/** GoalsCard keeps conversions visible as a full-width report even when the
 * site has no goal definitions yet. A configured goal with zero conversions is
 * a row; only the absence of definitions reaches the centered empty state. */
export function GoalsCard({ domain, range, filters, exact: exactAnswer }: Props) {
	const [ref, near] = useNearViewport<HTMLElement>();
	const report = useGoalsReport(domain, range, filters, exactAnswer, near);
	const rows = report.data?.rows ?? [];
	const peak = Math.max(1, ...rows.map((row) => row.unique_conversions));

	return (
		<section
			ref={ref}
			className="tint-languages flex h-card flex-col overflow-hidden rounded-md border border-line bg-card shadow-sm lg:col-span-2"
		>
			<header className="flex h-10 shrink-0 items-center gap-1.5 px-5">
				<h2 className="text-sm font-semibold text-body">{t("dashboard.report.goals.title")}</h2>
				<InfoDot text={t("dashboard.report.goals.caveat")} />
			</header>

			<div className="relative min-h-0 flex-1 px-5">
				{report.error ? (
					<Failure message={report.error} onRetry={report.reload} />
				) : !report.data ? (
					<Spinner label={t("dashboard.goals.loading")} />
				) : rows.length === 0 ? (
					<div className="flex h-full flex-col items-center justify-center gap-1 px-6 text-center">
						<p className="text-sm text-body">{t("dashboard.goals.empty")}</p>
						<p className="text-xs text-muted">{t("dashboard.goals.empty_hint")}</p>
					</div>
				) : (
					<>
						<div className="flex h-6 items-center text-[11px] font-medium tracking-wide text-muted uppercase">
							<span className="flex-1 truncate">{t("dashboard.column.goal")}</span>
							<span className="w-20 text-right sm:w-24">{t("dashboard.column.uniques")}</span>
							<span className="w-20 text-right sm:w-24">{t("dashboard.column.total")}</span>
							<span className="w-20 pr-1 text-right sm:w-24">{t("dashboard.column.conversion_rate")}</span>
						</div>

						<ul>
							{rows.map((row) => (
								<li key={row.goal.id} className="relative flex h-drawerrow items-center">
									{row.unique_conversions > 0 && <Bar share={row.unique_conversions / peak} />}
									<span className="relative min-w-0 flex-1 truncate pl-2 text-sm text-body" title={row.label}>
										{row.label}
									</span>
									<span className="tnum relative w-20 text-right text-sm text-body sm:w-24" title={exact(row.unique_conversions)}>
										{compact(row.unique_conversions)}
									</span>
									<span className="tnum relative w-20 text-right text-sm text-body sm:w-24" title={exact(row.total_conversions)}>
										{compact(row.total_conversions)}
									</span>
									<span
										className="tnum relative w-20 pr-1 text-right text-sm text-body sm:w-24"
										title={metricTitle("conversion_rate", row.conversion_rate)}
									>
										{metricAxisValue("conversion_rate", row.conversion_rate)}
									</span>
								</li>
							))}
						</ul>
					</>
				)}

				{report.loading && report.data && (
					<span aria-hidden="true" className="spinner-grace absolute inset-x-0 top-0 h-0.5 bg-accent/40" />
				)}
			</div>
		</section>
	);
}

/** useGoalsReport runs the one non-tabular report and holds its previous answer
 * while filters or the period change, matching the behavior of every stats
 * card above it. */
function useGoalsReport(
	domain: string,
	range: DateRange,
	filters: Filter[],
	exactAnswer: boolean,
	enabled: boolean,
): GoalsState {
	const [data, setData] = useState<GoalReport | null>(null);
	const [loading, setLoading] = useState(false);
	const [error, setError] = useState<string | null>(null);
	const [nonce, setNonce] = useState(0);
	const previous = useRef<GoalReport | null>(null);
	const key = JSON.stringify({ range, filters, exactAnswer });

	useEffect(() => {
		if (!enabled || !domain) return;

		const controller = new AbortController();
		let live = true;

		setLoading(true);
		setError(null);

		const request = JSON.parse(key) as { range: DateRange; filters: Filter[]; exactAnswer: boolean };
		goalsReport(
			domain,
			{ dateRange: request.range, filters: request.filters, exact: request.exactAnswer },
			controller.signal,
		)
			.then((response) => {
				if (!live) return;
				previous.current = response;
				setData(response);
				setLoading(false);
			})
			.catch((caught: unknown) => {
				if (!live || (caught instanceof DOMException && caught.name === "AbortError")) return;

				setData(previous.current);
				setError(caught instanceof QueryError || caught instanceof Error ? caught.message : t("dashboard.error.query_failed"));
				setLoading(false);
			});

		return () => {
			live = false;
			controller.abort();
		};
	}, [domain, key, enabled, nonce]);

	return { data, loading, error, reload: () => setNonce((value) => value + 1) };
}
