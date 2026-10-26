import type { Metadata } from 'next';
import Link from 'next/link';
import { Footer, Header } from '@/components/chrome';

export const metadata: Metadata = {
  title: 'Legal notice',
  description: 'Legal information for the website tutor-mcp.dev: publisher, hosting provider, license, liability, personal data and contact.',
  alternates: { canonical: '/mentions-legales/' },
  robots: { index: true, follow: true },
};

export default function Legal() {
  return (
    <>
      <Header />
      <main id="main">
        <div className="wrap legal-page">
          <header className="doc-head" style={{ paddingTop: 72 }}>
            <p className="eyebrow">Legal</p>
            <h1>Legal notice</h1>
            <p className="lead">Legal information for the website tutor-mcp.dev, the showcase of the open-source Tutor MCP project, in accordance with article 6 III of French law no. 2004-575 of 21 June 2004 on confidence in the digital economy (LCEN).</p>
            <p className="cap">Last updated: 10 May 2026.</p>
          </header>
          <nav aria-label="On this page">
            <a href="#publisher">Publisher</a><a href="#hosting">Hosting provider</a><a href="#license">License &amp; intellectual property</a><a href="#liability">Limitation of liability</a><a href="#data">Personal data &amp; cookies</a><a href="#contact">Contact</a>
          </nav>

          <h2 id="publisher">Publisher</h2>
          <p>This website is published in a personal, non-commercial capacity by Arnaud Guiovanna, a private individual and creator of the open-source Tutor MCP project.</p>
          <p>Publication director: Arnaud Guiovanna. Contact: <a href="mailto:arnaud@tutor-mcp.dev">arnaud@tutor-mcp.dev</a>.</p>
          <p>In accordance with article 6 III 2 of the LCEN, the publisher acts here as a non-professional individual. Their identity has been provided to the hosting provider, who may pass it on to any judicial authority requesting it.</p>

          <h2 id="hosting">Hosting provider</h2>
          <p>This website is hosted by:</p>
          <ul>
            <li>OVH SAS, simplified joint-stock company with capital of €50,000,000</li>
            <li>RCS Lille Métropole 424 761 419</li>
            <li>Registered office: 2 rue Kellermann, 59100 Roubaix, France</li>
            <li>Phone: 1007 (from France) — +33 9 72 10 10 07</li>
            <li>Website: <a href="https://www.ovh.com">www.ovh.com</a></li>
          </ul>

          <h2 id="license">License &amp; intellectual property</h2>
          <p>The Tutor MCP software is an open-source project released under the MIT license. The source code, its technical documentation and associated resources are publicly available on GitHub: <a href="https://github.com/ArnaudGuiovanna/tutor-mcp">github.com/ArnaudGuiovanna/tutor-mcp</a>.</p>
          <p>The MIT license grants any person the right to use, copy, modify, merge, publish, distribute, sublicense and/or sell the software, provided that the copyright notice and the license notice are kept in all copies or substantial portions of the software.</p>
          <p>The name “Tutor MCP”, the logo, the visual identity as well as the editorial content and illustrations specific to this website remain the property of their author. Any reproduction of these elements outside the scope of the MIT license (which only covers the software) requires the prior consent of the author.</p>
          <p>“Model Context Protocol” is an open specification maintained by the open-source community under the Linux Foundation. Tutor MCP is an independent implementation and is not affiliated with the protocol&apos;s maintainers.</p>

          <h2 id="liability">Limitation of liability</h2>
          <p>In accordance with the MIT license, the software is provided “AS IS”, without warranty of any kind, express or implied, including but not limited to the warranties of merchantability, fitness for a particular purpose and non-infringement. In no event shall the author or copyright holders be liable for any claim, damages or other liability, whether in an action of contract, tort or otherwise, arising from, out of or in connection with the software or the use or other dealings in the software.</p>
          <p>Regarding this website, the author endeavours to ensure the accuracy and timeliness of the information published, the content of which is purely informative. However, the author cannot guarantee that the content is complete or free from errors. The use of the information and content available on the site is the sole responsibility of the user.</p>
          <p>The author shall not be held liable for any direct or indirect, material or immaterial damage resulting from the use of the website, any unavailability, or any misuse by a third party. The site may link to third-party sites; the author exercises no control over these external resources and disclaims any responsibility for their content.</p>

          <h2 id="data">Personal data &amp; cookies</h2>
          <p>This website is static and does not collect any personal data. It uses no tracking cookies, no audience measurement tools and no third-party tracking services. No form or registration is offered on the site itself; contributions to the open-source project go through GitHub, which has its own terms of use and privacy policy.</p>

          <h2 id="contact">Contact</h2>
          <p>For any question about the site or the project: <a href="mailto:arnaud@tutor-mcp.dev">arnaud@tutor-mcp.dev</a>.</p>
          <p>For contributions, bug reports or technical discussions, please use <a href="https://github.com/ArnaudGuiovanna/tutor-mcp/issues">GitHub issues</a> or <a href="https://github.com/ArnaudGuiovanna/tutor-mcp/discussions">discussions</a>.</p>
          <p style={{ marginTop: 32 }}><Link href="/">← Back to home</Link></p>
        </div>
      </main>
      <Footer />
    </>
  );
}
