# Varroa Commercial License Agreement

**Template, version 1.0.** This is the standard agreement SB Studio LLC
offers to commercial licensees of Varroa. It is provided here for
transparency so procurement teams can review terms before contacting
us. It is not legal advice, and it is not an offer that can be accepted
by anything other than a signed copy countersigned by SB Studio LLC.

---

This Commercial License Agreement ("Agreement") is entered into as of
[EFFECTIVE DATE] between **SB Studio LLC** ("Licensor") and
**[CUSTOMER LEGAL NAME]** ("Customer").

## 1. Definitions

- **"Software"** means the Varroa software published at
  https://github.com/VarroaCI/varroa-jenkins, in source and binary
  form, including the operator, gateway, BFF, mite, update center,
  varroactl, frontend, Helm chart, and the VarroaSecurityRealm Jenkins
  plugin, together with updates released by Licensor during the Term.
- **"Managed Controller"** means one `Controller` custom resource
  reconciled by any installation of the Software operated by or for
  Customer, measured as the count returned by
  `kubectl get controllers --all-namespaces` across Customer's
  clusters.
- **"Licensed Count"** means the maximum number of Managed Controllers
  covered by this Agreement, as stated in the Order Form.
- **"Order Form"** means the ordering document referencing this
  Agreement that states the Licensed Count, fees, and Term.

## 2. License grant

Subject to timely payment of the fees in the Order Form, Licensor
grants Customer a non-exclusive, non-transferable, worldwide license
during the Term to:

1. use, run, and reproduce the Software for Customer's business,
   including hosting CI for Customer's affiliates and customers;
2. modify the Software and create derivative works for Customer's own
   use, **with no obligation to disclose or publish those
   modifications**; and
3. distribute the Software and Customer's modifications within
   Customer's organization and to contractors operating it on
   Customer's behalf.

This license is granted under Licensor's rights in the Software and is
independent of the GNU Affero General Public License v3.0. None of the
AGPL's conditions, including its source-availability and network-use
provisions, apply to Customer's exercise of the rights granted by this
Agreement.

## 3. Restrictions

Customer may not:

1. redistribute the Software or derivative works to third parties as a
   standalone product;
2. offer the Software to third parties as a managed or hosted CI
   product, except as expressly stated in the Order Form; or
3. remove copyright or license notices from the source.

Nothing in this Agreement restricts any rights Customer independently
holds under the AGPL-3.0.

## 4. Licensed Count and true-up

Customer self-reports its Managed Controller count. Within thirty (30)
days after each anniversary of the Effective Date, Customer will report
its then-current count. If the count exceeds the Licensed Count,
Customer will pay for the excess at the per-band rates stated in the
Order Form, prorated for the remainder of the Term, and the Licensed Count
is increased accordingly. There are no audits, agents, or telemetry;
reporting is on the honor system and this Section is the sole true-up
mechanism.

Users, build agents, executors, and builds are unlimited and are not
license metrics under this Agreement.

## 5. Fees

Fees are stated in the Order Form, quoted per Managed Controller in
banded annual rates (or as a flat-rate site license covering unlimited
Managed Controllers). Fees are exclusive of taxes and are
non-refundable except as stated in Section 8.

## 6. Support

During the Term, Licensor will provide the support described in
Exhibit A (severity definitions, response targets, and contact
channels). Support covers the two most recent minor releases of the
Software.

## 7. Term and termination

The initial Term is one (1) year from the Effective Date and renews for
successive one-year terms unless either party gives notice of
non-renewal at least thirty (30) days before renewal. Either party may
terminate for material breach uncured thirty (30) days after written
notice. On termination or expiry, the license in Section 2 ends;
Customer may continue using the Software under the AGPL-3.0, whose
terms then apply to any subsequent distribution or network offering of
modified versions.

## 8. Warranties and disclaimer

Licensor warrants that it has the right to grant the license in
Section 2. Customer's exclusive remedy for breach of this warranty is,
at Licensor's option, procurement of the necessary rights or a refund
of prepaid fees for the affected period. EXCEPT AS STATED IN THIS
SECTION, THE SOFTWARE IS PROVIDED "AS IS" AND LICENSOR DISCLAIMS ALL
OTHER WARRANTIES, EXPRESS OR IMPLIED, INCLUDING MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE, AND NON-INFRINGEMENT.

## 9. Limitation of liability

NEITHER PARTY IS LIABLE FOR INDIRECT, INCIDENTAL, SPECIAL,
CONSEQUENTIAL, OR PUNITIVE DAMAGES, OR FOR LOST PROFITS OR DATA. EACH
PARTY'S AGGREGATE LIABILITY UNDER THIS AGREEMENT IS LIMITED TO THE FEES
PAID OR PAYABLE BY CUSTOMER IN THE TWELVE (12) MONTHS PRECEDING THE
CLAIM. THESE LIMITS DO NOT APPLY TO CUSTOMER'S PAYMENT OBLIGATIONS OR
TO EITHER PARTY'S BREACH OF THE OTHER'S INTELLECTUAL PROPERTY RIGHTS.

## 10. General

Neither party may assign this Agreement without the other's consent,
except to a successor in a merger or sale of substantially all assets.
This Agreement is governed by the laws of [GOVERNING LAW STATE],
excluding conflict-of-law rules, and the parties consent to exclusive
jurisdiction of the courts located there. This Agreement plus the Order
Form and Exhibit A are the entire agreement and supersede prior
discussions. Amendments must be in writing and signed by both parties.

---

**SB Studio LLC**

Signature: ________________  Name/Title: ________________  Date: ______

**[CUSTOMER LEGAL NAME]**

Signature: ________________  Name/Title: ________________  Date: ______

---

## Exhibit A: Support

- **Channels:** email to support@varroa.dev; a private issue tracker on
  request.
- **Hours:** business days, [TIMEZONE].
- **Severity 1** (production fleet down, no workaround): first response
  within 1 business day.
- **Severity 2** (degraded operation, workaround exists): first
  response within 2 business days.
- **Severity 3** (questions, minor defects): first response within 5
  business days.
- Fixes ship as ordinary releases of the Software; Licensor does not
  maintain private forks per customer.
