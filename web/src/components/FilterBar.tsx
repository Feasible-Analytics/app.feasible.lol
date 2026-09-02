//
// FilterBar.tsx
// The pills, the dimension menu and the value editor.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import { useEffect, useMemo, useRef, useState } from "react";

import { properties as fetchProperties } from "../api/client";
import type { DateRange, Property, StatsRequest } from "../api/types";
import type { FilterLabels, FilterState, Operator } from "../lib/filters";
import { useDismiss } from "../lib/dom";
import { FILTERABLE, OPERATORS, dimensionLabel, pillMessage, remove, replace, valueOf } from "../lib/filters";
import { compact, exact } from "../lib/format";
import { t } from "../lib/i18n";
import { flagFor } from "../lib/labels";
import { useStats } from "../lib/useStats";
import { Flag, Spinner } from "./atoms";

/**
 * How many pills stay on the bar before the rest collapse.
 *
 * Four is where a row of pills stops being a summary of what you are looking at
 * and starts being a wall you have to read. The rest are one click away and the
 * count is on the button, so nothing is hidden — it is just not shouted.
 */
const VISIBLE_PILLS = 4;

/** How many values the autocomplete offers. Enough that the value somebody is
 *  looking for is nearly always in the list, few enough that the panel does not
 *  become its own scrolling report. */
const SUGGESTIONS = 25;

interface Props {
	domain: string;
	range: DateRange;
	filters: FilterState[];
	labels: FilterLabels;
	onChange: (filters: FilterState[], labels: FilterLabels) => void;
}

/** Which editor is open: a new filter, or the one at this index. */
interface Editing {
	index: number | null;
	dimension: string;
	operator: Operator;
	values: string[];
}

/** nameOf renders a dimension for a heading or a sentence. The registry hands
 *  back either a message id or a literal, because a custom property's name is
 *  the site's own data and has no translation. */
function nameOf(dimension: string): string {
	const label = dimensionLabel(dimension);

	return "id" in label ? t(label.id) : label.text;
}

/** describe renders a pill's sentence. One whole sentence per operator, so a
 *  translator can put the dimension, the verb and the value in whatever order
 *  their grammar needs. */
function describe(filter: FilterState, labels: FilterLabels): string {
	return t(pillMessage(filter), {
		dimension: nameOf(filter.dimension),
		value: readable(filter.dimension, filter.values[0] ?? "", labels),
		count: filter.values.length,
	});
}

/** readable is one value, with the blank spelled out. */
function readable(dimension: string, value: string, labels: FilterLabels): string {
	return valueOf(dimension, value, labels) || t("dashboard.value.not_set");
}

/** fullValues is the pill's title: every value spelled out, however many there
 *  are, so the shortened face never hides what is actually being asked. */
function fullValues(filter: FilterState, labels: FilterLabels): string {
	return filter.values.map((value) => readable(filter.dimension, value, labels)).join(", ");
}

/**
 * FilterBar is the whole filter surface.
 *
 * It renders nothing at all when there are no filters and nothing is open,
 * beyond the one button that starts a filter — a permanently empty toolbar
 * above the graph would cost forty pixels on every dashboard to advertise a
 * feature most sessions never use.
 */
export function FilterBar({ domain, range, filters, labels, onChange }: Props) {
	const [open, setOpen] = useState(false);
	const [editing, setEditing] = useState<Editing | null>(null);
	const [expanded, setExpanded] = useState(false);
	const wrap = useRef<HTMLDivElement>(null);
	const [customProperties, setCustomProperties] = useState<Property[]>([]);

	// The filter dimension menu is driven by the same enabled-property registry
	// as the Properties report. A raw property seen once is not silently exposed
	// until the site owner has chosen its scope and enabled it for analysis.
	useEffect(() => {
		if (!domain) return;
		const controller = new AbortController();
		let live = true;

		fetchProperties(domain, controller.signal)
			.then((found) => { if (live) setCustomProperties(found); })
			.catch(() => { if (live) setCustomProperties([]); });

		return () => { live = false; controller.abort(); };
	}, [domain]);

	useDismiss(wrap, open || editing !== null, () => {
		setOpen(false);
		setEditing(null);
	});

	// A pill removed from the overflow half must not leave the bar claiming
	// there are more filters hiding behind a button that no longer exists.
	useEffect(() => {
		if (filters.length <= VISIBLE_PILLS) setExpanded(false);
	}, [filters.length]);

	const shown = expanded ? filters : filters.slice(0, VISIBLE_PILLS);
	const hidden = filters.length - shown.length;

	/** apply writes an edited filter back, dropping it when every value was
	 *  cleared — an empty filter is one the engine refuses, and a pill reading
	 *  "Country is" is not a state anybody meant to reach. */
	const apply = (next: Editing, nextLabels: FilterLabels) => {
		setEditing(null);
		setOpen(false);

		if (next.values.length === 0) {
			if (next.index !== null) onChange(remove(filters, next.index), labels);
			return;
		}

		const filter: FilterState = { operator: next.operator, dimension: next.dimension, values: next.values };
		const merged = { ...labels, ...nextLabels };

		onChange(next.index === null ? [...filters, filter] : replace(filters, next.index, filter), merged);
	};

	return (
		<div ref={wrap} className="relative flex flex-wrap items-center gap-1.5">
			{shown.map((filter, index) => (
				<Pill
					key={`${filter.dimension}-${filter.operator}-${index}`}
					filter={filter}
					labels={labels}
					onEdit={filter.dimension === "event:goal" ? undefined : () =>
						setEditing({
							index,
							dimension: filter.dimension,
							operator: filter.operator,
							values: filter.values,
						})
					}
					onRemove={() => onChange(remove(filters, index), labels)}
				/>
			))}

			{hidden > 0 && (
				<button
					type="button"
					onClick={() => setExpanded(true)}
					title={filters
						.slice(VISIBLE_PILLS)
						.map((filter) => describe(filter, labels))
						.join(" · ")}
					className="h-control rounded-full border border-line px-2.5 text-xs font-medium text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover hover:text-body"
				>
					{t("dashboard.filter.more", { count: hidden })}
				</button>
			)}

			<button
				type="button"
				aria-expanded={open || editing !== null}
				onClick={() => {
					setEditing(null);
					setOpen((was) => !was);
				}}
				className="flex h-control items-center gap-1 rounded-full border border-dashed border-field px-2.5 text-xs font-medium text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:border-accent hover:text-accent"
			>
				<span aria-hidden="true">+</span> {t("dashboard.filter.add")}
			</button>

			{filters.length > 1 && (
				<button
					type="button"
					onClick={() => onChange([], {})}
					className="h-control px-1.5 text-xs text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:text-body"
				>
					{t("dashboard.filter.clear_all")}
				</button>
			)}

			{open && (
				<DimensionMenu
					properties={customProperties}
					onPick={(dimension) => {
						setOpen(false);
						setEditing({ index: null, dimension, operator: "is", values: [] });
					}}
				/>
			)}

			{editing && (
				<ValueEditor
					domain={domain}
					range={range}
					editing={editing}
					labels={labels}
					onCancel={() => setEditing(null)}
					onApply={apply}
				/>
			)}
		</div>
	);
}

/**
 * Pill is one filter, with its own remove button.
 *
 * The two halves are two buttons rather than one with a nested control, because
 * a button inside a button is invalid HTML that browsers repair differently, and
 * the repair is what decides whether the ✕ removes the filter or opens it.
 */
function Pill({
	filter,
	labels,
	onEdit,
	onRemove,
}: {
	filter: FilterState;
	labels: FilterLabels;
	onEdit?: () => void;
	onRemove: () => void;
}) {
	const text = describe(filter, labels);
	const glyph = filter.values.length === 1 ? flagFor(filter.dimension, filter.values[0] ?? "") : "";

	return (
		<span className="flex h-control items-center rounded-full border border-line bg-subtle pr-0.5 pl-2.5 text-xs">
			{onEdit ? (
				<button
					type="button"
					onClick={onEdit}
					title={t("dashboard.filter.edit_hint", { dimension: nameOf(filter.dimension), values: fullValues(filter, labels) })}
					className="flex max-w-56 items-center gap-1.5 truncate font-medium text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:text-accent"
				>
					<Flag glyph={glyph} /><span className="truncate">{text}</span>
				</button>
			) : (
				<span className="flex max-w-56 items-center gap-1.5 truncate font-medium text-body" title={fullValues(filter, labels)}><Flag glyph={glyph} /><span className="truncate">{text}</span></span>
			)}

			<button
				type="button"
				onClick={onRemove}
				aria-label={t("dashboard.filter.remove", { filter: text })}
				className="ml-1 flex size-5 shrink-0 items-center justify-center rounded-full text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover hover:text-body"
			>
				✕
			</button>
		</span>
	);
}

/** DimensionMenu lists everything that can be filtered, under the headings the
 *  cards already use, so somebody looking for "Browser" hunts in the same place
 *  they read it. */
function DimensionMenu({ onPick, properties }: { onPick: (dimension: string) => void; properties: Property[] }) {
	const groups = useMemo(() => {
		const seen: string[] = [];

		for (const entry of FILTERABLE) {
			if (entry.menu === false) continue;
			if (!seen.includes(entry.groupId)) seen.push(entry.groupId);
		}

		return seen;
	}, []);

	return (
		<div
			role="dialog"
			aria-label={t("dashboard.filter.menu_label")}
			className="scroll-thin absolute top-full left-0 z-40 mt-1.5 max-h-96 w-64 overflow-auto rounded-md border border-line bg-card p-1 shadow-lg"
		>
			{groups.map((group) => (
				<div key={group}>
					<p className="px-2.5 pt-2 pb-1 text-[10px] font-medium tracking-wide text-muted uppercase">
						{t(group)}
					</p>

					{FILTERABLE.filter((entry) => entry.menu !== false && entry.groupId === group).map((entry) => (
						<button
							key={entry.alias}
							type="button"
							onClick={() => onPick(entry.dimension)}
							className="w-full rounded-sm px-2.5 py-1.5 text-left text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
						>
							{t(entry.labelId)}
						</button>
					))}

					{group === "dashboard.filter.group.behaviour" && properties.map((property) => (
						<button
							key={`property:${property.id}`}
							type="button"
							onClick={() => onPick(`event:props:${property.name}`)}
							className="flex w-full items-center gap-2 rounded-sm px-2.5 py-1.5 text-left text-sm text-body transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover"
						>
							<span className="min-w-0 flex-1 truncate">{property.name}</span>
							<span className="text-[10px] text-muted">{property.scope}</span>
						</button>
					))}
				</div>
			))}
		</div>
	);
}

/**
 * ValueEditor is the panel that builds one filter.
 *
 * It is a popover anchored to the bar rather than a centred modal. A modal would
 * dim the dashboard the filter is about, and the one thing somebody needs while
 * choosing a value is to see the report they are choosing it from.
 */
function ValueEditor({
	domain,
	range,
	editing,
	labels,
	onCancel,
	onApply,
}: {
	domain: string;
	range: DateRange;
	editing: Editing;
	labels: FilterLabels;
	onCancel: () => void;
	onApply: (next: Editing, labels: FilterLabels) => void;
}) {
	const [operator, setOperator] = useState<Operator>(editing.operator);
	const [values, setValues] = useState<string[]>(editing.values);
	const [typed, setTyped] = useState("");
	const [search, setSearch] = useState("");
	const box = useRef<HTMLInputElement>(null);

	useEffect(() => box.current?.focus(), []);

	// The list is a real query, so it cannot chase every keystroke. The input is
	// local and the query only catches up once typing stops.
	useEffect(() => {
		const id = setTimeout(() => setSearch(typed.trim()), 250);

		return () => clearTimeout(id);
	}, [typed]);

	// A regex is written, not chosen, so there is nothing to suggest — and the
	// list of what exists would be a list of things the pattern does not match.
	const freeform = operator === "matches" || operator === "matches_not";

	const body: StatsRequest = {
		metrics: ["visitors"],
		date_range: range,
		dimensions: [editing.dimension],
		pagination: { limit: SUGGESTIONS },
		filters: search ? [["contains", editing.dimension, [search], { case_sensitive: false }]] : undefined,
	};

	const stats = useStats(domain, freeform ? null : body);
	const rows = stats.data?.results ?? [];

	/** collected is the labels this panel learned, so a country code chosen here
	 *  arrives on the recipient's screen with its name attached. */
	const collected = useMemo(() => {
		const found: FilterLabels = {};

		for (const value of values) {
			const rendered = valueOf(editing.dimension, value, labels);
			if (rendered && rendered !== value) found[value] = rendered;
		}

		return found;
	}, [values, editing.dimension, labels]);

	const toggleValue = (value: string) =>
		setValues((was) => (was.includes(value) ? was.filter((entry) => entry !== value) : [...was, value]));

	return (
		<div
			role="dialog"
			aria-label={t("dashboard.filter.dialog_label", { dimension: nameOf(editing.dimension) })}
			className="absolute top-full left-0 z-40 mt-1.5 flex w-80 flex-col rounded-md border border-line bg-card shadow-lg"
		>
			<div className="flex items-center gap-2 border-b border-line px-3 py-2">
				<span className="shrink-0 text-sm font-medium text-body">{nameOf(editing.dimension)}</span>

				<label className="ml-auto flex items-center">
					<span className="sr-only">{t("dashboard.filter.operator_label")}</span>
					<select
						value={operator}
						onChange={(event) => setOperator(event.target.value as Operator)}
						className="h-control cursor-pointer rounded-md border border-line bg-card px-1.5 text-xs text-body"
					>
						{OPERATORS.map((entry) => (
							<option key={entry.id} value={entry.id}>
								{t(entry.labelId)}
							</option>
						))}
					</select>
				</label>
			</div>

			<div className="px-3 py-2">
				<input
					ref={box}
					type="search"
					value={typed}
					onChange={(event) => setTyped(event.target.value)}
					onKeyDown={(event) => {
						// A value that is not in the list is still a value worth
						// filtering on: a page that only appeared yesterday, or a
						// regex. Enter takes whatever was typed.
						if (event.key !== "Enter" || !typed.trim()) return;

						event.preventDefault();
						toggleValue(typed.trim());
						setTyped("");
					}}
					placeholder={t(freeform ? "dashboard.filter.regex_placeholder" : "dashboard.filter.search_placeholder")}
					aria-label={t("dashboard.filter.value_label")}
					className="h-control w-full rounded-md border border-line bg-page px-2.5 text-sm text-body placeholder:text-muted"
				/>
			</div>

			{values.length > 0 && (
				<div className="flex flex-wrap gap-1 px-3 pb-2">
					{values.map((value) => (
						<button
							key={value}
							type="button"
							onClick={() => toggleValue(value)}
							aria-label={t("dashboard.filter.remove_value", {
								value: readable(editing.dimension, value, labels),
							})}
							className="flex max-w-full items-center gap-1 rounded-full bg-accent/10 px-2 py-0.5 text-xs font-medium text-accent"
						>
							<span className="truncate">{readable(editing.dimension, value, labels)}</span>
							<span aria-hidden="true">✕</span>
						</button>
					))}
				</div>
			)}

			{!freeform && (
				<div className="scroll-thin max-h-56 overflow-auto border-t border-line">
					{stats.error ? (
						<p className="px-3 py-3 text-xs text-down">{stats.error}</p>
					) : !stats.data ? (
						<div className="h-20">
							<Spinner label={t("dashboard.filter.loading_values")} />
						</div>
					) : rows.length === 0 ? (
						<p className="px-3 py-3 text-xs text-muted">{t("dashboard.filter.no_matches")}</p>
					) : (
						rows.map((row) => {
							const value = row.dimensions[0] ?? "";
							const on = values.includes(value);

							return (
								<button
									key={value}
									type="button"
									aria-pressed={on}
									onClick={() => toggleValue(value)}
									className={`flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm transition-colors duration-150 ease-[var(--ease-ui)] hover:bg-hover ${
										on ? "text-accent" : "text-body"
									}`}
								>
									<Flag glyph={flagFor(editing.dimension, value)} />
									<span className="min-w-0 flex-1 truncate">
										{readable(editing.dimension, value, labels)}
									</span>
									<span className="tnum shrink-0 text-xs text-muted"><span className="sr-only">{exact(row.metrics[0] ?? 0)}</span><span aria-hidden="true">{compact(row.metrics[0] ?? 0)}</span></span>
								</button>
							);
						})
					)}
				</div>
			)}

			<div className="flex items-center gap-2 border-t border-line px-3 py-2">
				<button
					type="button"
					onClick={onCancel}
					className="h-control rounded-md px-2.5 text-xs font-medium text-muted transition-colors duration-150 ease-[var(--ease-ui)] hover:text-body"
				>
					{t("dashboard.filter.cancel")}
				</button>

				<button
					type="button"
					onClick={() => onApply({ ...editing, operator, values }, collected)}
					className="ml-auto h-control rounded-md bg-accent px-3 text-xs font-medium text-fill-fg transition-opacity duration-150 ease-[var(--ease-ui)] hover:opacity-90 disabled:opacity-40"
					disabled={values.length === 0 && editing.index === null}
				>
					{t("dashboard.topbar.apply")}
				</button>
			</div>
		</div>
	);
}
