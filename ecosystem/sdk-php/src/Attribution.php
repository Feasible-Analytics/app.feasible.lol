<?php
//
// Attribution.php
// The server-side attribution overrides for an event with no referrer of its own.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible;

/**
 * The override fields the ingest endpoint honours for server-side callers. A
 * delayed or offline conversion — a webhook hours later, a phone order, a
 * refund — has no referrer of its own and would be filed as Direct forever, so
 * the campaign that earned it is passed explicitly instead.
 *
 * These are ignored on browser traffic, where the real referrer is authoritative.
 */
final class Attribution
{
    /**
     * Every field is optional because a caller usually knows one or two of them
     * — the campaign, say — and inventing values for the rest would be worse
     * than leaving them absent.
     */
    public function __construct(
        public readonly ?string $referrer = null,
        public readonly ?string $utmSource = null,
        public readonly ?string $utmMedium = null,
        public readonly ?string $utmCampaign = null,
        public readonly ?string $utmContent = null,
        public readonly ?string $utmTerm = null,
    ) {
    }

    /**
     * Renders only the fields that were set. Absent keys are omitted rather
     * than sent as null, because the endpoint treats a null as a value and
     * would overwrite what it derived with nothing.
     *
     * @return array<string, string>
     */
    public function toArray(): array
    {
        $pairs = [
            'referrer' => $this->referrer,
            'utm_source' => $this->utmSource,
            'utm_medium' => $this->utmMedium,
            'utm_campaign' => $this->utmCampaign,
            'utm_content' => $this->utmContent,
            'utm_term' => $this->utmTerm,
        ];

        $out = [];
        foreach ($pairs as $key => $value) {
            if ($value !== null && trim($value) !== '') {
                $out[$key] = $value;
            }
        }

        return $out;
    }
}
