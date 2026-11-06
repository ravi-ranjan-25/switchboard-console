export const metadata = { title: '[[.Title]]', description: '[[.Desc]]' };

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body style={{ fontFamily: 'system-ui, sans-serif', margin: '3rem auto', maxWidth: '40rem', padding: '0 1rem' }}>
        {children}
      </body>
    </html>
  );
}
