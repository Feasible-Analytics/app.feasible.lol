<?php
//
// Revenue.php
// The money one event reports.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible;

use Feasible\Exception\InvalidEventException;

/**
 * The `$` field of the wire payload. It is a type rather than a loose array so
 * that a currency typo fails at the call site: the server quietly ignores a
 * revenue object with no currency, and a revenue report that is silently zero
 * is the hardest kind of missing data to notice.
 */
final class Revenue
{
    /** The amount in major units, the way a payment provider reports a total. */
    public readonly int|float $amount;

    /** The ISO 4217 code, upper-cased. */
    public readonly string $currency;

    /**
     * Normalises the currency to the upper-case form the server stores, so that
     * "usd" and "USD" do not become two rows on the same report.
     */
    public function __construct(int|float $amount, string $currency)
    {
        $code = strtoupper(trim($currency));

        if (preg_match('/^[A-Z]{3}$/', $code) !== 1) {
            throw new InvalidEventException(sprintf(
                'revenue currency %s is not a three-letter ISO 4217 code, such as USD or GBP',
                var_export($currency, true)
            ));
        }

        $this->amount = $amount;
        $this->currency = $code;
    }

    /**
     * Renders the wire shape. The key names are the server's and are not
     * configurable, so they live in exactly one place.
     *
     * @return array{amount: int|float, currency: string}
     */
    public function toArray(): array
    {
        return ['amount' => $this->amount, 'currency' => $this->currency];
    }
}
